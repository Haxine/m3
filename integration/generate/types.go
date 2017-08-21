// SeriesDataPoint represents a single data point of a generated series of data
type SeriesDataPoint struct {
	ts.Datapoint
	ID ts.ID
}

// SeriesDataPointsByTime are a sorted list of SeriesDataPoints
type SeriesDataPointsByTime []SeriesDataPoint

	// SetWriteEmptyShards sets whether writes are done even for empty start periods
	SetWriteEmptyShards(bool) Options

	// WriteEmptyShards returns whether writes are done even for empty start periods
	WriteEmptyShards() bool
