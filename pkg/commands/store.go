package commands

import (
	"fmt"

	"github.com/beam-cloud/clip/pkg/archive"
	"github.com/spf13/cobra"
)

type StoreCmdOptions struct {
	InputFile  string
	RemotePath string
}

var StoreCmd = &cobra.Command{
	Use:   "store",
	Short: "Store a CLIP archive elsewhere and create an RCLIP archive",
}
var StoreS3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Generate an RCLIP archive backed by s3.",
	RunE:  runStoreS3,
}

var StoreLocalCmd = &cobra.Command{
	Use:   "local",
	Short: "Generate an RCLIP archive based by a local file.",
	RunE:  runStoreLocal,
}

type StoreS3Options struct {
	InputFile string
	AccessKey string
	SecretKey string
	Bucket    string
}

type StoreLocalOptions struct {
	InputFile  string
	OutputFile string
}

var storeS3Opts = &StoreS3Options{}
var storeLocalOpts = &StoreLocalOptions{}

func init() {
	StoreCmd.AddCommand(StoreS3Cmd)
	StoreCmd.AddCommand(StoreLocalCmd)

	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.InputFile, "input", "i", "", "Input CLIP archive path")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.InputFile, "output", "o", "", "Output RCLIP archive path")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.AccessKey, "access-key", "a", "", "S3 access key")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.SecretKey, "secret-key", "s", "", "S3 secret key")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.Bucket, "bucket", "b", "", "S3 bucket")
	StoreS3Cmd.MarkFlagRequired("input")
	StoreS3Cmd.MarkFlagRequired("output")

	StoreLocalCmd.Flags().StringVarP(&storeLocalOpts.InputFile, "input", "i", "", "Input CLIP archive path")
	StoreLocalCmd.Flags().StringVarP(&storeLocalOpts.OutputFile, "output", "o", "", "Output RCLIP archive path")
	StoreLocalCmd.MarkFlagRequired("input")
	StoreLocalCmd.MarkFlagRequired("output")
}

func runStoreS3(cmd *cobra.Command, args []string) error {
	fmt.Printf("Storing %s in S3 bucket %s...\n", storeS3Opts.InputFile, storeS3Opts.Bucket)

	return nil
}

func runStoreLocal(cmd *cobra.Command, args []string) error {
	fmt.Printf("Storing %s in local directory %s...\n", storeLocalOpts.InputFile, storeLocalOpts.OutputFile)
	a, err := archive.NewRClipArchiver(storeLocalOpts.InputFile)
	if err != nil {
		return err
	}

	err = a.Create(archive.ClipArchiverOptions{ArchivePath: storeLocalOpts.InputFile})
	if err != nil {
		return err
	}

	return nil
}