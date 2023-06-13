package commands

import (
	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/spf13/cobra"
)

var StoreCmd = &cobra.Command{
	Use:   "store",
	Short: "Store a CLIP archive in remote storage and create an RCLIP archive",
}

var StoreS3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Generate an RCLIP archive backed by s3.",
	RunE:  runStoreS3,
}

var storeS3Opts = &clip.StoreS3Options{}

func init() {
	StoreCmd.AddCommand(StoreS3Cmd)

	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.ArchivePath, "input", "i", "", "Input CLIP archive path")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.OutputFile, "output", "o", "", "Output RCLIP archive path")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.Bucket, "bucket", "b", "", "S3 bucket name")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.Key, "key", "k", "", "S3 bucket key (optional)")

	StoreS3Cmd.MarkFlagRequired("input")
	StoreS3Cmd.MarkFlagRequired("output")
	StoreS3Cmd.MarkFlagRequired("bucket")
}

func runStoreS3(cmd *cobra.Command, args []string) error {
	return clip.StoreS3(*storeS3Opts)
}
