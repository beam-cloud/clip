package commands

import (
	"os"
	"path/filepath"

	"github.com/beam-cloud/clip/pkg/archive"
	"github.com/beam-cloud/clip/pkg/common"
	"github.com/okteto/okteto/pkg/log"
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

type StoreS3Options struct {
	ArchivePath string
	OutputFile  string
	Bucket      string
	Key         string
}

var storeS3Opts = &StoreS3Options{}

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
	region := os.Getenv("AWS_REGION")

	// If no key is provided, use the base name of the input archive as key
	if storeS3Opts.Key == "" {
		storeS3Opts.Key = filepath.Base(storeS3Opts.ArchivePath)
	}

	storageInfo := &common.S3StorageInfo{Bucket: storeS3Opts.Bucket, Key: storeS3Opts.Key, Region: region}
	a, err := archive.NewRClipArchiver(storageInfo)
	if err != nil {
		return err
	}

	err = a.Create(storeS3Opts.ArchivePath, storeS3Opts.OutputFile)
	if err != nil {
		return err
	}

	log.Success("Done.")
	return nil
}
