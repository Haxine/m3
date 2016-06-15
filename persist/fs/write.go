	blockSize        time.Duration
	infoFd             *os.File
	indexFd            *os.File
	dataFd             *os.File
	checkpointFilePath string
	start        time.Time
	blockSize time.Duration,
		blockSize:        blockSize,
func (w *writer) Open(shard uint32, blockStart time.Time) error {
	w.start = blockStart
	w.checkpointFilePath = filepathFromTime(shardDir, blockStart, checkpointFileSuffix)
		w.openWritable,
			filepathFromTime(shardDir, blockStart, infoFileSuffix):  &w.infoFd,
			filepathFromTime(shardDir, blockStart, indexFileSuffix): &w.indexFd,
			filepathFromTime(shardDir, blockStart, dataFileSuffix):  &w.dataFd,
	if len(data) == 0 {
		return nil
	}
	return w.WriteAll(key, [][]byte{data})
}

func (w *writer) WriteAll(key string, data [][]byte) error {
	var size int64
	for _, d := range data {
		size += int64(len(d))
	}
	if size == 0 {
		return nil
	}
	entry.Idx = w.currIdx
	entry.Size = size
	endianness.PutUint64(w.idxData, uint64(w.currIdx))
	for _, d := range data {
		if err := w.writeData(d); err != nil {
			return err
		}
		Start:     xtime.ToNanoseconds(w.start),
		BlockSize: int64(w.blockSize),
		Entries:   w.currIdx,
	if err := closeFiles(w.infoFd, w.indexFd, w.dataFd); err != nil {
		return err
	}

	return w.writeCheckpointFile()
func (w *writer) writeCheckpointFile() error {
	fd, err := w.openWritable(w.checkpointFilePath)
	if err != nil {
		return err
	}
	fd.Close()
	return nil
}

func (w *writer) openWritable(filePath string) (*os.File, error) {
	fd, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, w.newFileMode)
	if err != nil {
		return nil, err
	return fd, nil