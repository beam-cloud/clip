package archive

type ClipArchiver struct {
	archive *ClipArchive
}

func NewClipArchiver() (*ClipArchiver, error) {
	return &ClipArchiver{
		archive: nil,
	}, nil
}

func (a *ClipArchiver) Create(sourcePath string, outputPath string) (*ClipArchive, error) {
	a.archive = NewClipArchive(sourcePath)
	err := a.archive.CreateIndex()
	if err != nil {
		return nil, err
	}

	err = a.archive.Dump(outputPath)
	if err != nil {
		return nil, err
	}

	return a.archive, nil
}

func (a *ClipArchiver) Extract(sourcePath string, outputPath string) (*ClipArchive, error) {
	a.archive = NewClipArchive(sourcePath)
	err := a.archive.CreateIndex()
	if err != nil {
		return nil, err
	}

	return a.archive, nil
}
