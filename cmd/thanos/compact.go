package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/improbable-eng/thanos/pkg/compact"
	"github.com/improbable-eng/thanos/pkg/objstore"
	"github.com/improbable-eng/thanos/pkg/objstore/gcs"
	"github.com/improbable-eng/thanos/pkg/objstore/s3"
	"github.com/improbable-eng/thanos/pkg/query/ui"
	"github.com/improbable-eng/thanos/pkg/runutil"
	"github.com/oklog/run"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/route"
	"github.com/prometheus/tsdb"
	"gopkg.in/alecthomas/kingpin.v2"
)

func registerCompact(m map[string]setupFunc, app *kingpin.Application, name string) {
	cmd := app.Command(name, "continously compacts blocks in an object store bucket")

	haltOnError := cmd.Flag("debug.halt-on-error", "halt the process if a critical compaction error is detected").
		Hidden().Bool()

	httpAddr := cmd.Flag("http-address", "listen host:port for HTTP endpoints").
		Default(defaultHTTPAddr).String()

	dataDir := cmd.Flag("data-dir", "data directory to cache blocks and process compactions").
		Default("./data").String()

	gcsBucket := cmd.Flag("gcs.bucket", "Google Cloud Storage bucket name for stored blocks.").
		PlaceHolder("<bucket>").String()

	var s3config s3.Config

	cmd.Flag("s3.bucket", "S3-Compatible API bucket name for stored blocks.").
		PlaceHolder("<bucket>").Envar("S3_BUCKET").StringVar(&s3config.Bucket)

	cmd.Flag("s3.endpoint", "S3-Compatible API endpoint for stored blocks.").
		PlaceHolder("<api-url>").Envar("S3_ENDPOINT").StringVar(&s3config.Endpoint)

	cmd.Flag("s3.access-key", "Access key for an S3-Compatible API.").
		PlaceHolder("<key>").Envar("S3_ACCESS_KEY").StringVar(&s3config.AccessKey)

	cmd.Flag("s3.secret-key", "Secret key for an S3-Compatible API.").
		PlaceHolder("<key>").Envar("S3_SECRET_KEY").StringVar(&s3config.SecretKey)

	cmd.Flag("s3.insecure", "Whether to use an insecure connection with an S3-Compatible API.").
		Default("false").Envar("S3_INSECURE").BoolVar(&s3config.Insecure)

	syncDelay := cmd.Flag("sync-delay", "minimum age of blocks before they are being processed.").
		Default("2h").Duration()

	m[name] = func(g *run.Group, logger log.Logger, reg *prometheus.Registry, tracer opentracing.Tracer) error {
		return runCompact(g, logger, reg,
			*httpAddr,
			*dataDir,
			*gcsBucket,
			&s3config,
			*syncDelay,
			*haltOnError,
		)
	}
}

func runCompact(
	g *run.Group,
	logger log.Logger,
	reg *prometheus.Registry,
	httpAddr string,
	dataDir string,
	gcsBucket string,
	s3Config *s3.Config,
	syncDelay time.Duration,
	haltOnError bool,
) error {
	var (
		bkt    objstore.Bucket
		bucket string
	)

	halted := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "thanos_compactor_halted",
		Help: "Set to 1 if the compactor halted due to an unexpected error",
	})
	halted.Set(0)
	reg.MustRegister(halted)

	if gcsBucket != "" {
		gcsClient, err := storage.NewClient(context.Background())
		if err != nil {
			return errors.Wrap(err, "create GCS client")
		}
		bkt = gcs.NewBucket(gcsBucket, gcsClient.Bucket(gcsBucket), reg)
		bucket = gcsBucket
	} else if s3Config.Validate() == nil {
		b, err := s3.NewBucket(s3Config, reg)
		if err != nil {
			return errors.Wrap(err, "create s3 client")
		}

		bkt = b
		bucket = s3Config.Bucket
	} else {
		return errors.New("no valid GCS or S3 configuration supplied")
	}

	bkt = objstore.BucketWithMetrics(bucket, bkt, reg)

	sy, err := compact.NewSyncer(logger, reg, bkt, syncDelay)
	if err != nil {
		return err
	}
	// Start cycle of syncing blocks from the bucket and garbage collecting the bucket.
	{
		// Instantiate the compactor with different time slices. Timestamps in TSDB
		// are in milliseconds.
		comp, err := tsdb.NewLeveledCompactor(reg, logger, []int64{
			int64(2 * time.Hour / time.Millisecond),
			int64(8 * time.Hour / time.Millisecond),
			int64(2 * 24 * time.Hour / time.Millisecond),
			int64(14 * 24 * time.Hour / time.Millisecond),
		}, nil)
		if err != nil {
			return errors.Wrap(err, "create compactor")
		}

		ctx, cancel := context.WithCancel(context.Background())

		g.Add(func() error {
			return runutil.Repeat(5*time.Minute, ctx.Done(), func() error {
				if err := sy.SyncMetas(ctx); err != nil {
					level.Error(logger).Log("msg", "sync failed", "err", err)
				}
				if err := sy.GarbageCollect(ctx); err != nil {
					level.Error(logger).Log("msg", "garbage collection failed", "err", err)
				}
				groups, err := sy.Groups()
				if err != nil {
					return errors.Wrap(err, "build compaction groups")
				}
				for _, g := range groups {
					os.RemoveAll(dataDir)
					// While we do all compactions sequentially we just compact within the top-level dir.
					if _, err := g.Compact(ctx, dataDir, comp); err == nil {
						continue
					}
					level.Error(logger).Log("msg", "compaction failed", "err", err)
					// The HaltError type signals that we hit a critical bug and should block
					// for investigation.
					if compact.IsHaltError(err) {
						if haltOnError {
							level.Error(logger).Log("msg", "critical error detected; halting")
							halted.Set(1)
							select {}
						} else {
							return errors.Wrap(err, "critical error detected")
						}
					}
				}
				return nil
			})
		}, func(error) {
			cancel()
		})
	}
	// Start metric and profiling endpoints.
	{
		router := route.New()
		ui.New(logger, nil).Register(router)

		mux := http.NewServeMux()
		registerMetrics(mux, reg)
		registerProfile(mux)
		mux.Handle("/", router)

		l, err := net.Listen("tcp", httpAddr)
		if err != nil {
			return errors.Wrapf(err, "listen on address %s", httpAddr)
		}

		g.Add(func() error {
			return errors.Wrap(http.Serve(l, mux), "serve query")
		}, func(error) {
			l.Close()
		})
	}

	level.Info(logger).Log("msg", "starting compact node")
	return nil
}
