package sinker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/streamingfast/dstore"
	"github.com/streamingfast/logging"
	"github.com/streamingfast/shutter"
	sink "github.com/streamingfast/substreams-sink"
	pbdatabase "github.com/streamingfast/substreams-sink-database-changes/pb/sf/substreams/sink/database/v1"
	"github.com/streamingfast/substreams-sink-postgres/bundler"
	"github.com/streamingfast/substreams-sink-postgres/bundler/writer"
	"github.com/streamingfast/substreams-sink-postgres/db"
	"github.com/streamingfast/substreams-sink-postgres/state"
	pbsubstreamsrpc "github.com/streamingfast/substreams/pb/sf/substreams/rpc/v2"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type BulkSinker struct {
	*shutter.Shutter
	*sink.Sinker
	destFolder string

	fileBundlers map[string]*bundler.Bundler
	stopBlock    uint64

	// cursor
	stateStore state.Store
	bundleSize uint64

	loader *db.Loader
	logger *zap.Logger
	tracer logging.Tracer

	stats *Stats
}

func NewBulkSinker(
	sink *sink.Sinker,
	destFolder string,
	workingDir string,
	bundleSize uint64,
	bufferSize uint64,
	loader *db.Loader,
	logger *zap.Logger,
	tracer logging.Tracer,
) (*BulkSinker, error) {
	blockRange := sink.BlockRange()
	if blockRange == nil || blockRange.EndBlock() == nil {
		return nil, fmt.Errorf("sink must have a stop block defined")
	}

	// create cursor
	stateStorePath := filepath.Join(destFolder, "state.yaml")
	stateFileDirectory := filepath.Dir(stateStorePath)
	if err := os.MkdirAll(stateFileDirectory, os.ModePerm); err != nil {
		return nil, fmt.Errorf("create state file directories: %w", err)
	}

	stateStore, err := state.NewFileStateStore(stateStorePath)
	if err != nil {
		return nil, fmt.Errorf("new file state store: %w", err)
	}

	s := &BulkSinker{
		Shutter: shutter.New(),
		Sinker:  sink,

		fileBundlers: make(map[string]*bundler.Bundler),
		stopBlock:    *blockRange.EndBlock(),

		loader: loader,
		logger: logger,
		tracer: tracer,

		stateStore: stateStore,
		bundleSize: bundleSize,

		stats: NewStats(logger),
	}

	csvOutputStore, err := dstore.NewStore(destFolder, "csv", "", false)
	if err != nil {
		return nil, err
	}
	tables := s.loader.GetAvailableTablesInSchemaList()

	for _, table := range tables {
		columns := s.loader.GetColumnsForTable(table)
		fb, err := getBundler(table, s.Sinker.BlockRange().StartBlock(), s.stopBlock, bundleSize, bufferSize, csvOutputStore, workingDir, logger, columns)
		if err != nil {
			return nil, err
		}
		s.fileBundlers[table] = fb
	}

	return s, nil
}

func (b *BulkSinker) GetCursor() (*sink.Cursor, error) {
	return b.stateStore.ReadCursor()
}

func (s *BulkSinker) Run(ctx context.Context) {
	cursor, err := s.GetCursor()
	if err != nil && !errors.Is(err, db.ErrCursorNotFound) {
		s.Shutdown(fmt.Errorf("unable to retrieve cursor: %w", err))
		return
	}

	s.Sinker.OnTerminating(s.Shutdown)
	s.OnTerminating(func(err error) {
		s.Sinker.Shutdown(err)
		s.stats.LogNow()
		s.logger.Info("csv sinker terminating", zap.Stringer("last_block_written", s.stats.lastBlock))
		s.stats.Close()
		s.CloseAllFileBundlers(err)
	})

	s.stats.OnTerminated(func(err error) { s.Shutdown(err) })

	logEach := 15 * time.Second
	if s.logger.Core().Enabled(zap.DebugLevel) {
		logEach = 5 * time.Second
	}

	s.stats.Start(logEach, cursor)

	s.logger.Info("starting postgres sink",
		zap.Duration("stats_refresh_each", logEach),
		zap.Stringer("restarting_at", cursor.Block()),
		zap.String("database", s.loader.GetDatabase()),
		zap.String("schema", s.loader.GetSchema()),
	)

	uploadContext := context.Background()
	for _, fb := range s.fileBundlers {
		fb.Launch(uploadContext)
	}
	s.Sinker.Run(ctx, cursor, s)
}

func (s *BulkSinker) HandleBlockScopedData(ctx context.Context, data *pbsubstreamsrpc.BlockScopedData, isLive *bool, cursor *sink.Cursor) error {
	output := data.Output

	if output.Name != s.OutputModuleName() {
		return fmt.Errorf("received data from wrong output module, expected to received from %q but got module's output for %q", s.OutputModuleName(), output.Name)
	}

	dbChanges := &pbdatabase.DatabaseChanges{}
	mapOutput := output.GetMapOutput()
	if !mapOutput.MessageIs(dbChanges) && mapOutput.TypeUrl != "type.googleapis.com/sf.substreams.database.v1.DatabaseChanges" {
		return fmt.Errorf("mismatched message type: trying to unmarshal unknown type %q", mapOutput.MessageName())
	}

	// We do not use UnmarshalTo here because we need to parse an older proto type and
	// UnmarshalTo enforces the type check. So we check manually the `TypeUrl` above and we use
	// `Unmarshal` instead which only deals with the bytes value.
	if err := proto.Unmarshal(mapOutput.Value, dbChanges); err != nil {
		return fmt.Errorf("unmarshal database changes: %w", err)
	}

	if err := s.dumpDatabaseChangesIntoCSV(dbChanges); err != nil {
		return fmt.Errorf("apply database changes: %w", err)
	}

	// we set the cursor before rolling because roll process shutdown the system
	// TODO update to use queue
	if cursor.Block().Num()%s.bundleSize == 0 {
		s.stateStore.SetCursor(cursor)
		state, err := s.stateStore.GetState()
		if err != nil {
			s.Shutdown(fmt.Errorf("unable to get state: %w", err))
			return fmt.Errorf("unable to get state: %w", err)
		}
		if err := state.Save(); err != nil {
			s.Shutdown(fmt.Errorf("unable to save state: %w", err))
			return fmt.Errorf("unable to save state: %w", err)
		}
	}

	s.rollAllBundlers(ctx, data.Clock.Number, cursor)

	return nil
}

func (s *BulkSinker) dumpDatabaseChangesIntoCSV(dbChanges *pbdatabase.DatabaseChanges) error {
	for _, change := range dbChanges.TableChanges {
		if !s.loader.HasTable(change.Table) {
			return fmt.Errorf(
				"your Substreams sent us a change for a table named %s we don't know about on %s (available tables: %s)",
				change.Table,
				s.loader.GetIdentifier(),
				s.loader.GetAvailableTablesInSchema(),
			)
		}

		var fields map[string]string
		switch u := change.PrimaryKey.(type) {
		case *pbdatabase.TableChange_Pk:
			var err error
			fields, err = s.loader.GetPrimaryKey(change.Table, u.Pk)
			if err != nil {
				return err
			}
		case *pbdatabase.TableChange_CompositePk:
			fields = u.CompositePk.Keys
		default:
			return fmt.Errorf("unknown primary key type: %T", change.PrimaryKey)
		}
		table := change.Table
		tableBundler, ok := s.fileBundlers[table]
		if !ok {
			return fmt.Errorf("cannot get bundler writer for table %s", table)
		}
		switch change.Operation {
		case pbdatabase.TableChange_CREATE:
			// add fields
			for _, field := range change.Fields {
				fields[field.Name] = field.NewValue
			}
			data, _ := bundler.CSVEncode(fields)
			if !tableBundler.HeaderWritten {
				tableBundler.Writer().Write(tableBundler.Header)
				tableBundler.HeaderWritten = true
			}
			tableBundler.Writer().Write(data)
		case pbdatabase.TableChange_UPDATE:
		case pbdatabase.TableChange_DELETE:
		default:
			return fmt.Errorf("Currently, we only support append only databases")
		}
	}

	return nil
}

func (s *BulkSinker) rollAllBundlers(ctx context.Context, blockNum uint64, cursor *sink.Cursor) {
	var wg sync.WaitGroup
	for _, entityBundler := range s.fileBundlers {
		wg.Add(1)

		eb := entityBundler
		go func() {
			if err := eb.Roll(ctx, blockNum); err != nil {
				// no worries, Shutdown can and will be called multiple times
				if errors.Is(err, bundler.ErrStopBlockReached) {
					err = nil
				}
				s.Shutdown(err)
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func (s *BulkSinker) CloseAllFileBundlers(err error) {
	var wg sync.WaitGroup
	for _, fb := range s.fileBundlers {
		wg.Add(1)
		f := fb
		go func() {
			f.Shutdown(err)
			<-f.Terminated()
			wg.Done()
		}()
	}
	wg.Wait()
}

func (s *BulkSinker) HandleBlockUndoSignal(ctx context.Context, data *pbsubstreamsrpc.BlockUndoSignal, cursor *sink.Cursor) error {
	return fmt.Errorf("received undo signal but there is no handling of undo, this is because you used `--undo-buffer-size=0` which is invalid right now")
}

func getBundler(table string, startBlock, stopBlock, bundleSize, bufferSize uint64, baseOutputStore dstore.Store, workingDir string, logger *zap.Logger, columns []string) (*bundler.Bundler, error) {
	boundaryWriter := writer.NewBufferedIO(
		bufferSize,
		filepath.Join(workingDir, table),
		writer.FileTypeCSV,
		logger.With(zap.String("table_name", table)),
	)
	subStore, err := baseOutputStore.SubStore(table)
	if err != nil {
		return nil, err
	}

	sort.Strings(columns)

	header := []byte(strings.Join(columns, ",") + "\n")
	fb, err := bundler.New(bundleSize, stopBlock, boundaryWriter, subStore, logger, header)
	if err != nil {
		return nil, err
	}
	fb.Start(startBlock)
	return fb, nil
}
