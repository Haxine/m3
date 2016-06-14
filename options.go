package memtsdb

import (
	"time"

	"code.uber.internal/infra/memtsdb/x/metrics"

	"github.com/uber-common/bark"
)

// NowFn is the function supplied to determine "now"
type NowFn func() time.Time

// DatabaseOptions is a set of database options
type DatabaseOptions interface {
	// EncodingTszPooled sets tsz encoding with pooling and returns a new DatabaseOptions
	EncodingTszPooled(bufferBucketAllocSize, series int) DatabaseOptions

	// Logger sets the logger and returns a new DatabaseOptions
	Logger(value bark.Logger) DatabaseOptions

	// GetLogger returns the logger
	GetLogger() bark.Logger

	// MetricsScope sets the metricsScope and returns a new DatabaseOptions
	MetricsScope(value metrics.Scope) DatabaseOptions

	// GetMetricsScope returns the metricsScope
	GetMetricsScope() metrics.Scope

	// BlockSize sets the blockSize and returns a new DatabaseOptions
	BlockSize(value time.Duration) DatabaseOptions

	// GetBlockSize returns the blockSize
	GetBlockSize() time.Duration

	// NewEncoderFn sets the newEncoderFn and returns a new DatabaseOptions
	// TODO(r): now that we have an encoder pool consider removing newencoderfn being required
	NewEncoderFn(value NewEncoderFn) DatabaseOptions

	// GetNewEncoderFn returns the newEncoderFn
	GetNewEncoderFn() NewEncoderFn

	// NewDecoderFn sets the newDecoderFn and returns a new DatabaseOptions
	NewDecoderFn(value NewDecoderFn) DatabaseOptions

	// GetNewDecoderFn returns the newDecoderFn
	GetNewDecoderFn() NewDecoderFn

	// NowFn sets the nowFn and returns a new DatabaseOptions
	NowFn(value NowFn) DatabaseOptions

	// GetNowFn returns the nowFn
	GetNowFn() NowFn

	// BufferFuture sets the bufferFuture and returns a new DatabaseOptions
	BufferFuture(value time.Duration) DatabaseOptions

	// GetBufferFuture returns the bufferFuture
	GetBufferFuture() time.Duration

	// BufferPast sets the bufferPast and returns a new DatabaseOptions
	BufferPast(value time.Duration) DatabaseOptions

	// GetBufferPast returns the bufferPast
	GetBufferPast() time.Duration

	// BufferFlush sets the bufferFlush and returns a new DatabaseOptions
	BufferFlush(value time.Duration) DatabaseOptions

	// GetBufferFlush returns the bufferFlush
	GetBufferFlush() time.Duration

	// BufferBucketAllocSize sets the bufferBucketAllocSize and returns a new DatabaseOptions
	BufferBucketAllocSize(value int) DatabaseOptions

	// GetBufferBucketAllocSize returns the bufferBucketAllocSize
	GetBufferBucketAllocSize() int

	// RetentionPeriod sets how long we intend to keep data in memory.
	RetentionPeriod(value time.Duration) DatabaseOptions

	// RetentionPeriod is how long we intend to keep data in memory.
	GetRetentionPeriod() time.Duration

	// NewBootstrapFn sets the newBootstrapFn and returns a new DatabaseOptions
	NewBootstrapFn(value NewBootstrapFn) DatabaseOptions

	// GetBootstrapFn returns the newBootstrapFn
	GetBootstrapFn() NewBootstrapFn

	// BytesPool sets the bytesPool and returns a new DatabaseOptions
	BytesPool(value BytesPool) DatabaseOptions

	// GetBytesPool returns the bytesPool
	GetBytesPool() BytesPool

	// ContextPool sets the contextPool and returns a new DatabaseOptions
	ContextPool(value ContextPool) DatabaseOptions

	// GetContextPool returns the contextPool
	GetContextPool() ContextPool

	// EncoderPool sets the encoderPool and returns a new DatabaseOptions
	EncoderPool(value EncoderPool) DatabaseOptions

	// GetEncoderPool returns the encoderPool
	GetEncoderPool() EncoderPool

	// IteratorPool sets the iteratorPool and returns a new DatabaseOptions
	IteratorPool(value IteratorPool) DatabaseOptions

	// GetIteratorPool returns the iteratorPool
	GetIteratorPool() IteratorPool
}