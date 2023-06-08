package archive

type ClipArchiver struct {
	archive *ClipArchive
}

type ClipArchiverOptions struct {
	Compress   bool
	SourcePath string
	OutputFile string
}

func NewClipArchiver() (*ClipArchiver, error) {
	return &ClipArchiver{
		archive: nil,
	}, nil
}

func (a *ClipArchiver) Create(opts ClipArchiverOptions) (*ClipArchive, error) {
	a.archive = NewClipArchive(opts.SourcePath)
	err := a.archive.CreateIndex()
	if err != nil {
		return nil, err
	}

	err = a.archive.Dump(opts.OutputFile)
	if err != nil {
		return nil, err
	}

	return a.archive, nil
}

func (a *ClipArchiver) Extract(opts ClipArchiverOptions) (*ClipArchive, error) {
	a.archive = NewClipArchive(opts.SourcePath)

	// TODO: extract the archive
	return a.archive, nil
}
