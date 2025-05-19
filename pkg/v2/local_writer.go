package clipv2

import "io"

type LocalChunkWriter struct {
	io.WriteCloser
}

func (l *LocalChunkWriter) WaitForCompletion() error {
	return nil
}
