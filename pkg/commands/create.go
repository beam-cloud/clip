package commands

import (
	"fmt"

	"github.com/beam-cloud/clip/pkg/archive"
	log "github.com/okteto/okteto/pkg/log"
	"github.com/spf13/cobra"
)

type CreateCmdOptions struct {
	InputPath  string
	OutputPath string
	Verbose    bool
}

var createOpts = &CreateCmdOptions{}

var CreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create an archive from the specified path",
	Run:   runCreate,
}

func init() {
	CreateCmd.Flags().StringVarP(&createOpts.InputPath, "input", "i", "", "Input directory to archive")
	CreateCmd.Flags().StringVarP(&createOpts.OutputPath, "output", "o", "test.clip", "Output file for the archive")
	CreateCmd.Flags().BoolVarP(&createOpts.Verbose, "verbose", "v", false, "Verbose output")
	CreateCmd.MarkFlagRequired("input")
}

func runCreate(cmd *cobra.Command, args []string) {
	log.Spinner("Archiving...")
	log.StartSpinner()
	defer log.StopSpinner()

	log.Information(fmt.Sprintf("Creating a new archive from directory: %s", createOpts.InputPath))

	a := archive.NewClipArchiver()
	err := a.Create(archive.ClipArchiverOptions{
		SourcePath: createOpts.InputPath,
		OutputFile: createOpts.OutputPath,
		Verbose:    createOpts.Verbose,
	})

	if err != nil {
		log.Fail("An error occurred while creating the archive: %s\n", err)
	} else {
		log.Success("Archive created successfully.")
	}
}
