package main

import (
	"net/http"
	"time"

	_ "github.com/lib/pq"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	. "github.com/streamingfast/cli"
	"github.com/streamingfast/dmetrics"
	"go.uber.org/zap"
)

// Version value, injected via go build `ldflags` at build time
var version = "dev"

func main() {
	Run("substreams-sink-postgres", "Substreams Postgres Sink",
		sinkRunCmd,
		sinkSetupCmd,
		sinkToolsCmd,
		sinkFirstLoadCmd,
		injectCSVCmd,

		ConfigureViper("SINK_POSTGRES"),
		ConfigureVersion(version),

		PersistentFlags(func(flags *pflag.FlagSet) {
			flags.Duration("delay-before-start", 0, "[Operator] Amount of time to wait before starting any internal processes, can be used to perform to maintenance on the pod before actually letting it starts")
			flags.String("metrics-listen-addr", "localhost:9102", "[Operator] If non-empty, the process will listen on this address for Prometheus metrics request(s)")
			flags.String("pprof-listen-addr", "localhost:6060", "[Operator] If non-empty, the process will listen on this address for pprof analysis (see https://golang.org/pkg/net/http/pprof/)")
		}),
		AfterAllHook(func(cmd *cobra.Command) {
			cmd.PersistentPreRun = preStart
		}),
	)
}

func preStart(_ *cobra.Command, _ []string) {
	delay := viper.GetDuration("global-delay-before-start")
	if delay > 0 {
		zlog.Info("sleeping to respect delay before start setting", zap.Duration("delay", delay))
		time.Sleep(delay)
	}

	if v := viper.GetString("global-metrics-listen-addr"); v != "" {
		zlog.Debug("starting prometheus metrics server", zap.String("listen_addr", v))
		go dmetrics.Serve(v)
	}

	if v := viper.GetString("global-pprof-listen-addr"); v != "" {
		go func() {
			zlog.Debug("starting pprof server", zap.String("listen_addr", v))
			err := http.ListenAndServe(v, nil)
			if err != nil {
				zlog.Debug("unable to start profiling server", zap.Error(err), zap.String("listen_addr", v))
			}
		}()
	}
}
