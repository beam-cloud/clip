package commands

import (
	"fmt"

	"github.com/beam-cloud/clip/pkg/archive"
	log "github.com/okteto/okteto/pkg/log"
	"github.com/spf13/cobra"
)

type ExtractCmdOptions struct {
	InputFile  string
	OutputPath string
	Verbose    bool
}

var extractOpts = &ExtractCmdOptions{}

var ExtractCmd = &cobra.Command{
	Use:   "extract",
	Short: "Extract an archive to the specified path",
	Run:   runExtract,
}

func init() {
	ExtractCmd.Flags().StringVarP(&extractOpts.InputFile, "input", "i", "", "Input file to extract")
	ExtractCmd.Flags().StringVarP(&extractOpts.OutputPath, "output", "o", ".", "Output path for the extraction")
	ExtractCmd.Flags().BoolVarP(&extractOpts.Verbose, "verbose", "v", false, "Verbose output")
	ExtractCmd.MarkFlagRequired("input")
}

func runExtract(cmd *cobra.Command, args []string) {
	log.Spinner("Extracting...")
	log.StartSpinner()
	defer log.StopSpinner()

	log.Information(fmt.Sprintf("Extracting archive: %s", extractOpts.InputFile))

	a := archive.NewClipArchiver()
	err := a.Extract(archive.ClipArchiverOptions{
		ArchivePath: extractOpts.InputFile,
		OutputPath:  extractOpts.OutputPath,
		Verbose:     extractOpts.Verbose,
	})

	if err != nil {
		log.Fail("An error occurred while extracting the archive: %s\n", err)
	} else {
		log.Success("Archive extracted successfully.")
	}
}
