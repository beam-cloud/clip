package main

import (
	"log"

	"github.com/beam-cloud/clip/pkg/clip"
)

func main() {
	clipArchiver := clip.NewClipArchiver()

	opts := clip.ClipArchiverOptions{
		SourcePath: "./test",
		OutputFile: "test.clip",
	}

	err := clipArchiver.Create(opts)
	if err != nil {
		log.Fatalf("Failed to create archive: %v", err)
	}
}
