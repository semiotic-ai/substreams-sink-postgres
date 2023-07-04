package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/streamingfast/cli"
	. "github.com/streamingfast/cli"
	"github.com/streamingfast/cli/sflags"
	"github.com/streamingfast/shutter"
	sink "github.com/streamingfast/substreams-sink"
	"github.com/streamingfast/substreams-sink-postgres/db"
	"github.com/streamingfast/substreams-sink-postgres/sinker"
	"go.uber.org/zap"
)

var generateCsvCmd = Command(generateCsvE,
	"generate-csv <psql_dsn> <endpoint> <manifest> <module> <dest-folder> <stop>",
	"Runs first load sink process",
	Description(`
		This command is going to fetch all known cursors from the database. In the database,
		a cursor is saved per module's hash which mean if you update your '.spkg', you might
		end up with multiple cursors for different module.

		This command will list all of them.
	`),
	ExactArgs(6),
	Flags(func(flags *pflag.FlagSet) {
		sink.AddFlagsToSet(flags)

		flags.Uint64("bundle-size", 1000, "Size of output bundle, in blocks")
		flags.String("start-block", "", "Start processing at this block instead of the substreams initial block")
		flags.String("working-dir", "./workdir", "Path to local folder used as working directory")
		flags.String("on-module-hash-mistmatch", "error", FlagDescription(`
			What to do when the module hash in the manifest does not match the one in the database, can be 'error', 'warn' or 'ignore'

			- If 'error' is used (default), it will exit with an error explaining the problem and how to fix it.
			- If 'warn' is used, it does the same as 'ignore' but it will log a warning message when it happens.
			- If 'ignore' is set, we pick the cursor at the highest block number and use it as the starting point. Subsequent
			updates to the cursor will overwrite the module hash in the database.
		`),
		)
	}),
	OnCommandErrorLogAndExit(zlog),
)

func generateCsvE(cmd *cobra.Command, args []string) error {
	app := shutter.New()

	sink.RegisterMetrics()
	sinker.RegisterMetrics()

	ctx, cancelApp := context.WithCancel(cmd.Context())
	app.OnTerminating(func(_ error) {
		cancelApp()
	})

	psqlDSN := args[0]
	endpoint := args[1]
	manifestPath := args[2]
	outputModuleName := args[3]
	destFolder := args[4]
	stopBlock := args[5]

	startBlock := sflags.MustGetString(cmd, "start-block") // empty string by default makes valid ':endBlock' range

	bundleSize := sflags.MustGetUint64(cmd, "bundle-size")
	bufferSize := uint64(10 * 1024) // too high, this wrecks havoc
	workingDir := sflags.MustGetString(cmd, "working-dir")
	blockRange := startBlock + ":" + stopBlock

	moduleMismatchMode, err := db.ParseOnModuleHashMismatch(sflags.MustGetString(cmd, "on-module-hash-mistmatch"))
	cli.NoError(err, "invalid mistmatch mode")

	flushInterval := time.Duration(bundleSize)

	dbLoader, err := db.NewLoader(psqlDSN, flushInterval, moduleMismatchMode, zlog, tracer)
	if err != nil {
		return fmt.Errorf("new psql loader: %w", err)
	}

	if err := dbLoader.LoadTables(); err != nil {
		var e *db.CursorError
		if errors.As(err, &e) {
			fmt.Printf("Error validating the cursors table: %s\n", e)
			fmt.Println("You can use the following sql schema to create a cursors table")
			fmt.Println()
			fmt.Println(dbLoader.GetCreateCursorsTableSQL())
			fmt.Println()
			return fmt.Errorf("invalid cursors table")
		}
		return fmt.Errorf("load psql table: %w", err)
	}

	sink, err := sink.NewFromViper(
		cmd,
		"sf.substreams.sink.database.v1.DatabaseChanges,sf.substreams.database.v1.DatabaseChanges",
		endpoint, manifestPath, outputModuleName, blockRange,
		zlog,
		tracer,
	)
	if err != nil {
		return fmt.Errorf("unable to setup sinker: %w", err)
	}

	generateCSVSinker, err := sinker.NewGenerateCSVSinker(sink, destFolder, workingDir, bundleSize, bufferSize, dbLoader, zlog, tracer)
	if err != nil {
		return fmt.Errorf("unable to setup generate csv sinker: %w", err)
	}

	generateCSVSinker.OnTerminated(app.Shutdown)
	app.OnTerminating(func(_ error) {
		generateCSVSinker.Shutdown(nil)
	})

	go func() {
		generateCSVSinker.Run(ctx)
	}()

	zlog.Info("ready, waiting for signal to quit")

	signalHandler, isSignaled, _ := cli.SetupSignalHandler(0*time.Second, zlog)
	select {
	case <-signalHandler:
		go app.Shutdown(nil)
		break
	case <-app.Terminating():
		zlog.Info("run terminating", zap.Bool("from_signal", isSignaled.Load()), zap.Bool("with_error", app.Err() != nil))
		break
	}

	zlog.Info("waiting for run termination")
	select {
	case <-app.Terminated():
	case <-time.After(30 * time.Second):
		zlog.Warn("application did not terminate within 30s")
	}

	if err := app.Err(); err != nil {
		return err
	}

	zlog.Info("run terminated gracefully")
	return nil
}
