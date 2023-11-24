package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/streamingfast/cli"
	. "github.com/streamingfast/cli"
	sink "github.com/streamingfast/substreams-sink"
	"github.com/streamingfast/substreams-sink-sql/db"
)

var sinkToolsCmd = Group(
	"tools",
	"Tools for developers and operators",

	PersistentFlags(func(flags *pflag.FlagSet) {
		flags.String("dsn", "", "The DSN string used to connect to your database")
	}),

	Group("cursor", "Tools related to cursor handling (read/write)",
		Command(toolsReadCursorE,
			"read",
			"[Operator] Read active cursor(s) from database, if present",
			Description(`
				This command is going to fetch all known cursors from the database. In the database,
				a cursor is saved per module's hash which mean if you update your '.spkg', you might
				end up with multiple cursors for different module.

				This command will list all of them.
			`),
		),

		Command(toolsWriteCursorE,
			"write <module_hash> <cursor>",
			"[Operator] Write a new active cursor for a given module's hash in the database",
			Description(`
				**Warning** This can screw up 'substreams-sink-sql' state, use only if you know. what you are doing.

				This command is going to write a new cursor in the database for the given module's
				hash. The command update the current cursor if it exists or insert a new one if
				none already exist.
			`),
		),

		Command(toolsDeleteCursorE,
			"delete <module_hash>",
			"[Operator] Delete the active cursor for a given module's hash in the database",
			Description(`
				**Warning** This can screw up 'substreams-sink-sql' state, use only if you know. what you are doing.

				This command is going to delete the cursor in the database for the given module's
				hash. If the cursor does not exist, the command assume it is correctly deleted.
			`),
			RangeArgs(0, 1),
			Flags(func(flags *pflag.FlagSet) {
				flags.BoolP("all", "a", false, "Delete all active cursors")
			}),
		),
	),
)

func toolsReadCursorE(cmd *cobra.Command, _ []string) error {
	loader := toolsCreateLoader()

	out, err := loader.GetAllCursors(cmd.Context())
	cli.NoError(err, "Unable to get all cursors")

	if len(out) == 0 {
		fmt.Println("No cursor(s) present in the database")
		return nil
	}

	for id, cursor := range out {
		fmt.Printf("Module %s: Block %s [%s]\n", id, cursor.Block(), cursorToShortString(cursor))
	}

	return nil
}

func toolsWriteCursorE(cmd *cobra.Command, args []string) error {
	loader := toolsCreateLoader()

	moduleHash := args[0]
	opaqueCursor := args[1]

	cli.Ensure(moduleHash != "", "The <module_hash> cannot be empty")
	cli.Ensure(len(moduleHash) == 40, "The <module_hash> must be exactly 40 characters long")

	cursor, err := sink.NewCursor(opaqueCursor)
	cli.NoError(err, "The <cursor> is invalid")

	err = loader.UpdateCursor(cmd.Context(), nil, moduleHash, cursor)
	if err != nil {
		if errors.Is(err, db.ErrCursorNotFound) {
			err = loader.InsertCursor(cmd.Context(), moduleHash, cursor)
			cli.NoError(err, "Unable to insert cursor")
		}

		cli.NoError(err, "Unable to update cursor")
	}

	fmt.Println("Cursor written successfully")
	fmt.Printf("- Block %s\n", cursor.Block())
	fmt.Printf("- Head Block %s\n", cursor.HeadBlock)
	fmt.Printf("- LIB Block %s\n", cursor.LIB)
	fmt.Printf("- Step %q\n", cursor.Step)
	fmt.Printf("- Cursor %q\n", cursorToShortString(cursor))
	return nil
}

func toolsDeleteCursorE(cmd *cobra.Command, args []string) error {
	loader := toolsCreateLoader()

	moduleHash := ""
	if !viper.GetBool("tools-cursor-delete-all") {
		cli.Ensure(len(args) == 1, "Module hash is required, if you want to delete all cursors, use --all to avoid specifying a module's hash")

		moduleHash := args[0]

		cli.Ensure(moduleHash != "", "The <module_hash> cannot be empty")
		cli.Ensure(len(moduleHash) == 40, "The <module_hash> must be exactly 40 characters long")
	}

	if moduleHash == "" {
		deletedCount, err := loader.DeleteAllCursors(cmd.Context())
		cli.NoError(err, "Unable to delete cursor")

		fmt.Printf("Deleted %d cursor(s) successfully\n", deletedCount)
	} else {
		err := loader.DeleteCursor(cmd.Context(), moduleHash)
		if err != nil && !errors.Is(err, db.ErrCursorNotFound) {
			cli.NoError(err, "Unable to delete cursor")
		}

		fmt.Println("Cursor delete successfully")
	}

	return nil
}

func toolsCreateLoader() *db.Loader {
	dsn := viper.GetString("tools-global-dsn")
	loader, err := db.NewLoader(dsn, 0, db.OnModuleHashMismatchIgnore, nil, zlog, tracer)
	cli.NoError(err, "Unable to instantiate database manager from DSN %q", dsn)

	if err := loader.LoadTables(); err != nil {
		var systemTableError *db.SystemTableError
		if errors.As(err, &systemTableError) {
			fmt.Printf("Error validating the system table: %s\n", systemTableError)
			fmt.Println("Did you run setup ?")
			os.Exit(1)
		}

		cli.NoError(err, "Unable to load table information from database")
	}

	return loader
}

func cursorToShortString(in *sink.Cursor) string {
	cursor := in.String()
	if len(cursor) > 12 {
		cursor = cursor[0:6] + "..." + cursor[len(cursor)-6:]
	}

	return cursor
}
