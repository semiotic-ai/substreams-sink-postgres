package state

import (
	"context"

	"github.com/streamingfast/bstream"
	sink "github.com/streamingfast/substreams-sink"
	"github.com/streamingfast/substreams-sink-postgres/bundler/writer"
)

type Store interface {
	Start(context.Context)
	Close()
	NewBoundary(*bstream.Range)
	ReadCursor(context.Context) (*sink.Cursor, error)
	SetCursor(*sink.Cursor)
	GetState() (Saveable, error)
	UploadCursor(state Saveable)
	Shutdown(error)
}

type Saveable interface {
	Save() error
	GetUploadeable() writer.Uploadeable
}
