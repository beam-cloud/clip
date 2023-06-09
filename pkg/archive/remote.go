package archive

import "log"

type RClipArchiver struct {
	clipArchiver *ClipArchiver
	RemotePath   string
}

type RClipArchive struct {
}

func NewRClipArchiver(archivePath string) (*RClipArchiver, error) {
	return &RClipArchiver{
		clipArchiver: NewClipArchiver(),
	}, nil
}

func (rca *RClipArchiver) Create(opts ClipArchiverOptions) error {
	metadata, err := rca.clipArchiver.ExtractMetadata(ClipArchiverOptions{
		ArchivePath: opts.ArchivePath,
	})
	if err != nil {
		return err
	}

	log.Println("loaded metadata: ", metadata)

	return nil
}

func (rca *RClipArchiver) Load(opts ClipArchiverOptions) error {

	return nil
}
