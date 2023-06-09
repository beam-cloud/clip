package archive

import "log"

type RClipArchive struct {
	*ClipArchiver
	RemotePath string
}

func NewRClipArchive(archivePath string) (*RClipArchive, error) {
	ca := &ClipArchiver{}

	a := NewClipArchiver()
	metadata, err := a.ExtractMetadata(ClipArchiverOptions{
		ArchivePath: archivePath,
	})
	if err != nil {
		return nil, err
	}

	log.Printf("header: %+v", metadata.Header)

	return &RClipArchive{
		ClipArchiver: ca,
	}, nil
}

func (rca *RClipArchive) Create(opts ClipArchiverOptions) error {

	return nil
}

func (rca *RClipArchive) Load(opts ClipArchiverOptions) error {

	return nil
}
