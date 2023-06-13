package commands

import (
	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/spf13/cobra"
)

var extractOpts = &clip.ExtractOptions{}

var ExtractCmd = &cobra.Command{
	Use:   "extract",
	Short: "Extract an archive to the specified path",
	RunE:  runExtract,
}

func init() {
	ExtractCmd.Flags().StringVarP(&extractOpts.InputFile, "input", "i", "", "Input file to extract")
	ExtractCmd.Flags().StringVarP(&extractOpts.OutputPath, "output", "o", ".", "Output path for the extraction")
	ExtractCmd.Flags().BoolVarP(&extractOpts.Verbose, "verbose", "v", false, "Verbose output")
	ExtractCmd.MarkFlagRequired("input")
}

func runExtract(cmd *cobra.Command, args []string) error {
	return clip.ExtractClipArchive(*extractOpts)
}
