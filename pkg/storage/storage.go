package storage

type ClipStorageInterface interface {
	ReadIndex(string) error
	ReadFile() (int, error)
	Put() (int, error)
}

func NewClipStorage() {

}
