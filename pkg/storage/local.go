package storage

type LocalClipStorage struct {
}

func (s *LocalClipStorage) ReadIndex() error {
	return nil
}

func (s *LocalClipStorage) ReadFile(start int64, end int64) (int, error) {

	return 0, nil
}
