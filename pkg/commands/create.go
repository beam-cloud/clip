package commands

import (
	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/spf13/cobra"
)

var createOpts = &clip.CreateOptions{}

var CreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create an archive from the specified path",
	RunE:  runCreate,
}

func init() {
	CreateCmd.Flags().StringVarP(&createOpts.InputPath, "input", "i", "", "Input directory to archive")
	CreateCmd.Flags().StringVarP(&createOpts.OutputPath, "output", "o", "test.clip", "Output file for the archive")
	CreateCmd.Flags().BoolVarP(&createOpts.Verbose, "verbose", "v", false, "Verbose output")
	CreateCmd.MarkFlagRequired("input")
}

func runCreate(cmd *cobra.Command, args []string) error {
	return clip.CreateArchive(*createOpts)
}
