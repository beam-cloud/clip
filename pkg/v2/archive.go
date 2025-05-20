package clipv2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/hanwen/go-fuse/v2/fuse"
	log "github.com/rs/zerolog/log"
	"golang.org/x/sys/unix"

	common "github.com/beam-cloud/clip/pkg/common"

	"github.com/tidwall/btree"
)

// Define chunking constants
const (
	DefaultMaxChunkSize = 32 * 1024 * 1024 // 32MB
	ChunkSuffix         = ".cblock"
	HeaderLenSize       = 8
)

func init() {
	gob.Register(&common.ClipNode{})
	gob.Register(common.ClipArchiveHeader{})
}

type StorageMode string

const (
	StorageModeLocal StorageMode = "local"
	StorageModeS3    StorageMode = "s3"
)

type InodeGenerator struct {
	current uint64
}

func (ig *InodeGenerator) Next() uint64 {
	ig.current++
	return ig.current
}

type ClipV2ArchiverOptions struct {
	Verbose      bool
	Compress     bool
	SourcePath   string
	LocalPath    string
	OutputPath   string
	MaxChunkSize int64
	IndexID      string
	StorageType  common.StorageMode
	S3Config     common.S3StorageInfo
}

type ClipV2Archiver struct {
	ClipV2ArchiverOptions
	chunkSize int64
}

func NewClipV2Archiver(opts ClipV2ArchiverOptions) *ClipV2Archiver {
	return &ClipV2Archiver{
		ClipV2ArchiverOptions: opts,
		chunkSize:             DefaultMaxChunkSize,
	}
}

func (ca *ClipV2Archiver) SetChunkSize(size int64) {
	if size > 0 {
		ca.chunkSize = size
	} else {
		ca.chunkSize = DefaultMaxChunkSize
		log.Warn().Msgf("Invalid chunk size %d provided, using default %d", size, ca.chunkSize)
	}
}

// Create creates a new ClipV2 archive from the source path.
func (ca *ClipV2Archiver) Create() error {
	ctx := context.Background()
	if ca.SourcePath == "" {
		return fmt.Errorf("SourcePath must be specified")
	}

	if ca.StorageType == common.StorageModeLocal {
		if err := os.MkdirAll(ca.LocalPath, 0755); err != nil {
			return fmt.Errorf("failed to create archive local directory %s: %w", ca.LocalPath, err)
		}
	}

	ca.SetChunkSize(ca.MaxChunkSize)

	index := newIndex()
	if err := ca.populateIndex(index, ca.SourcePath); err != nil {
		return fmt.Errorf("failed to populate index: %w", err)
	}
	log.Info().Msgf("Index populated with %d items", index.Len())

	var chunkNames []string
	chunkNames, err := ca.writePackedChunks(index)
	if err != nil {
		return fmt.Errorf("failed to write packed chunks: %w", err)
	}

	indexWriter, err := ca.newIndexWriter(ctx)
	if err != nil {
		return err
	}

	indexBytes, err := ca.EncodeIndex(index)
	if err != nil {
		return fmt.Errorf("failed to encode index: %w", err)
	}

	chunkListBytes, err := ca.EncodeChunkList(chunkNames)
	if err != nil {
		return fmt.Errorf("failed to encode chunk list: %w", err)
	}

	chunkListLength := int64(len(chunkListBytes))
	indexLength := int64(len(indexBytes))

	// Chunk list begins after the header
	chunkPos := int64(common.ClipHeaderLength)
	// Index begins after the chunk list
	indexPos := chunkPos + chunkListLength

	header := ClipV2ArchiveHeader{
		ClipFileFormatVersion: ClipV2FileFormatVersion,
		IndexLength:           indexLength,
		IndexPos:              indexPos,
		ChunkListLength:       chunkListLength,
		ChunkPos:              chunkPos,
		ChunkSize:             ca.chunkSize,
	}

	copy(header.StartBytes[:], common.ClipFileStartBytes)

	// FIXME: This logic is setup weirdly. The storage type should
	// be stored regardless and length shouldn't be used as the determining
	// factor on mounting.
	if ca.StorageType == common.StorageModeS3 {
		var storageType [12]byte
		copy(storageType[:], []byte(ca.StorageType))
		header.StorageInfoLength = int64(len(ca.StorageType))
		header.StorageInfoType = storageType
	}

	encodedHeaderBytes, err := ca.EncodeHeader(&header)
	if err != nil {
		return fmt.Errorf("failed to encode header: %w", err)
	}

	if _, err := indexWriter.Write(encodedHeaderBytes); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	if _, err := indexWriter.Write(chunkListBytes); err != nil {
		return fmt.Errorf("failed to write chunk list: %w", err)
	}

	if _, err := indexWriter.Write(indexBytes); err != nil {
		return fmt.Errorf("failed to write index data: %w", err)
	}

	indexWriter.Close()
	err = indexWriter.WaitForCompletion()
	if err != nil {
		return fmt.Errorf("failed to wait for index writer completion: %w", err)
	}

	log.Info().Msgf("Successfully created index %s (%d nodes, %d chunks) chunks in %s",
		ca.IndexID, index.Len(), len(chunkNames), ca.LocalPath)
	return nil
}

// ExtractArchive extracts the archive from the given path into a new ClipV2Archive object.
func (ca *ClipV2Archiver) ExtractArchive(ctx context.Context) (*ClipV2Archive, error) {
	archiveReader, err := ca.newIndexReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 index reader: %w", err)
	}

	header, err := ca.extractHeader(archiveReader)
	if err != nil {
		return nil, fmt.Errorf("failed to extract header from archive: %w", err)
	}

	chunks, err := ca.extractChunkList(archiveReader, header.ChunkListLength)
	if err != nil {
		return nil, fmt.Errorf("failed to extract chunk list from archive: %w", err)
	}

	index, err := ca.extractIndex(header, archiveReader)
	if err != nil {
		return nil, fmt.Errorf("failed to extract index from archive: %w", err)
	}

	return &ClipV2Archive{
		Header: *header,
		Index:  index,
		Chunks: chunks,
	}, nil
}

// ExpandLocalArchive expands the archive into the given output path.
func (ca *ClipV2Archiver) ExpandLocalArchive(ctx context.Context) error {
	if ca.LocalPath == "" || ca.OutputPath == "" {
		return fmt.Errorf("LocalPath and OutputPath must be specified for extraction")
	}

	file, err := ca.newIndexReader(ctx)
	if err != nil {
		return err
	}
	defer file.Close()

	archive, err := ca.ExtractArchive(ctx)
	if err != nil {
		return fmt.Errorf("failed to extract archive from %s: %w", filepath.Join(ca.LocalPath, "index.clip"), err)
	}
	header := &archive.Header
	chunks := archive.Chunks
	index := archive.Index

	chunkSize := header.ChunkSize
	if chunkSize <= 0 {
		return fmt.Errorf("invalid chunk size %d found in archive header", chunkSize)
	}
	log.Info().Msgf("Starting extraction using chunk size %d and %d chunks", chunkSize, len(chunks))

	if err := os.MkdirAll(ca.OutputPath, 0755); err != nil {
		return fmt.Errorf("failed to create base output directory %s: %w", ca.OutputPath, err)
	}

	var errors []error
	minNode, _ := index.Min()
	index.Ascend(minNode, func(node *common.ClipNode) bool {
		destPath := path.Join(ca.OutputPath, node.Path)

		if ca.Verbose {
			log.Info().Msgf("Extracting... %s", node.Path)
		}

		parentDir := filepath.Dir(destPath)
		if parentDir != "." && parentDir != "/" && parentDir != destPath {
			if err := os.MkdirAll(parentDir, 0755); err != nil {
				errors = append(errors, fmt.Errorf("mkdirall %s: %w", parentDir, err))
				return false
			}
		}

		switch node.NodeType {
		case common.DirNode:
			if err := os.Mkdir(destPath, fs.FileMode(node.Attr.Mode&0777)|os.ModeDir); err != nil && !os.IsExist(err) {
				errors = append(errors, fmt.Errorf("mkdir %s: %w", destPath, err))
				return true
			}
		case common.SymLinkNode:
			if err := os.Symlink(node.Target, destPath); err != nil {
				errors = append(errors, fmt.Errorf("symlink %s: %w", destPath, err))
				return true
			}
		case common.FileNode:
			if node.DataPos < 0 || node.DataLen < 0 {
				errors = append(errors, fmt.Errorf("skipped incomplete file %s", node.Path))
				return true
			}
			expectedFileSize := node.DataLen
			if expectedFileSize == 0 {
				emptyFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(node.Attr.Mode&0777))
				if err != nil {
					errors = append(errors, fmt.Errorf("create empty file %s: %w", destPath, err))
				} else {
					emptyFile.Close()
				}
				if err == nil {
					if err := os.Chmod(destPath, fs.FileMode(node.Attr.Mode&0777)); err != nil {
						errors = append(errors, fmt.Errorf("chmod empty %s: %w", destPath, err))
					}
					if err := os.Lchown(destPath, int(node.Attr.Uid), int(node.Attr.Gid)); err != nil {
						errors = append(errors, fmt.Errorf("lchown empty %s: %w", destPath, err))
					}
				}
				return true
			}

			outFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(node.Attr.Mode&0777))
			if err != nil {
				errors = append(errors, fmt.Errorf("create file %s: %w", destPath, err))
				return true
			}

			var bytesWrittenTotal int64 = 0
			startOffset := node.DataPos
			endOffset := node.DataPos + expectedFileSize

			startChunk := startOffset / chunkSize
			endChunk := (endOffset - 1) / chunkSize

			var reconstructErr error
			for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
				chunkPath := filepath.Join(ca.LocalPath, ca.IndexID, "chunks", chunks[chunkIdx])

				chunkFile, err := os.Open(chunkPath)
				if err != nil {
					reconstructErr = fmt.Errorf("failed open chunk %s for %s: %w", chunkPath, destPath, err)
					break
				}

				var offsetInChunk int64
				if chunkIdx == startChunk {
					offsetInChunk = startOffset % chunkSize
				} else {
					offsetInChunk = 0
				}

				remainingSpaceInChunk := chunkSize - offsetInChunk
				remainingBytesInFile := expectedFileSize - bytesWrittenTotal

				bytesToReadFromChunk := min(remainingSpaceInChunk, remainingBytesInFile)

				if bytesToReadFromChunk <= 0 {
					chunkFile.Close()
					continue
				}

				chunkDataSegment := make([]byte, bytesToReadFromChunk)
				n, readAtErr := chunkFile.ReadAt(chunkDataSegment, offsetInChunk)
				chunkFile.Close()

				if readAtErr != nil && readAtErr != io.EOF {
					reconstructErr = fmt.Errorf("read err chunk %s offset %d: %w", chunkPath, offsetInChunk, readAtErr)
					break
				}
				if int64(n) < bytesToReadFromChunk {
					reconstructErr = fmt.Errorf("chunk %s truncated: read %d, expected %d", chunkPath, n, bytesToReadFromChunk)
					break
				}

				_, writeAtErr := outFile.WriteAt(chunkDataSegment[:n], bytesWrittenTotal)
				if writeAtErr != nil {
					reconstructErr = fmt.Errorf("write err file %s offset %d: %w", destPath, bytesWrittenTotal, writeAtErr)
					break
				}
				bytesWrittenTotal += int64(n)
			}

			closeErr := outFile.Close()
			if reconstructErr != nil {
				errors = append(errors, reconstructErr)
				return true
			}
			if closeErr != nil {
				errors = append(errors, fmt.Errorf("close file %s: %w", destPath, closeErr))
				return true
			}

			if bytesWrittenTotal != expectedFileSize {
				errors = append(errors, fmt.Errorf("size mismatch %s", destPath))
			}
		default:
			log.Warn().Msgf("Skipping extraction - unknown node type %s for %s", node.NodeType, node.Path)
		}

		return true
	})

	if len(errors) > 0 {
		return fmt.Errorf("extraction completed with errors: %w", errors[0])
	}

	log.Info().Msgf("Successfully extracted archive to %s", ca.OutputPath)
	return nil
}

// EncodeHeader encodes the header into a byte slice.
func (ca *ClipV2Archiver) EncodeHeader(header *ClipV2ArchiveHeader) ([]byte, error) {
	var headerDataBuf bytes.Buffer
	enc := gob.NewEncoder(&headerDataBuf)
	if err := enc.Encode(header); err != nil {
		return nil, fmt.Errorf("failed to gob encode header data: %w", err)
	}
	headerDataBytes := headerDataBuf.Bytes()
	headerDataLen := uint64(len(headerDataBytes))

	finalBuf := bytes.NewBuffer(make([]byte, 0, HeaderLenSize+int(headerDataLen)))

	if err := binary.Write(finalBuf, binary.LittleEndian, headerDataLen); err != nil {
		return nil, fmt.Errorf("failed to write header length prefix: %w", err)
	}

	if _, err := finalBuf.Write(headerDataBytes); err != nil {
		return nil, fmt.Errorf("failed to write header data bytes: %w", err)
	}

	return finalBuf.Bytes(), nil
}

// DecodeHeader decodes the header from the given reader.
func (ca *ClipV2Archiver) DecodeHeader(reader io.Reader) (*ClipV2ArchiveHeader, error) {
	var headerDataLen uint64

	if err := binary.Read(reader, binary.LittleEndian, &headerDataLen); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("failed to read header length prefix (file too short?): %w", err)
		}
		return nil, fmt.Errorf("failed to read header length prefix: %w", err)
	}

	if headerDataLen > 1024*1024*1024 {
		return nil, fmt.Errorf("header length prefix (%d) seems unreasonably large", headerDataLen)
	}

	headerBytes := make([]byte, headerDataLen)
	if _, err := io.ReadFull(reader, headerBytes); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("failed to read %d header data bytes (file truncated?): %w", headerDataLen, err)
		}
		return nil, fmt.Errorf("failed to read %d header data bytes: %w", headerDataLen, err)
	}

	buf := bytes.NewBuffer(headerBytes)
	dec := gob.NewDecoder(buf)
	header := new(ClipV2ArchiveHeader)
	if err := dec.Decode(header); err != nil {
		return nil, fmt.Errorf("failed to gob decode header data: %w", err)
	}

	return header, nil
}

// EncodeIndex encodes the index into a byte slice.
func (ca *ClipV2Archiver) EncodeIndex(index *btree.BTreeG[*common.ClipNode]) ([]byte, error) {
	var nodes []*common.ClipNode
	minNode, _ := index.Min()
	index.Ascend(minNode, func(a *common.ClipNode) bool {
		nodes = append(nodes, a)
		return true
	})

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(nodes); err != nil {
		return nil, fmt.Errorf("gob encoding index nodes failed: %w", err)
	}

	return buf.Bytes(), nil
}

// EncodeChunkList encodes the chunk list into a byte slice.
func (ca *ClipV2Archiver) EncodeChunkList(chunkList []string) ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(chunkList); err != nil {
		return nil, fmt.Errorf("gob encoding chunk list failed: %w", err)
	}

	return buf.Bytes(), nil
}

// populateIndex populates the index with the nodes (representing files and directories) in the source path
func (ca *ClipV2Archiver) populateIndex(index *btree.BTreeG[*common.ClipNode], sourcePath string) error {
	root := &common.ClipNode{
		Path:     "/",
		NodeType: common.DirNode,
		Attr: fuse.Attr{
			Mode: uint32(os.ModeDir | 0755),
		},
	}
	index.Set(root)

	inodeGen := &InodeGenerator{current: 1}
	inodeMap := make(map[string]uint64)

	err := filepath.WalkDir(sourcePath, func(currentPath string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Warn().Err(err).Msgf("Error accessing path %s, skipping", currentPath)
			return filepath.SkipDir
		}

		if currentPath == sourcePath {
			return nil
		}

		var target string = ""
		var nodeType common.ClipNodeType
		info, err := d.Info()
		if err != nil {
			log.Warn().Err(err).Msgf("Error getting info for %s, skipping", currentPath)
			return nil
		}

		fileMode := info.Mode()
		if fileMode.IsDir() {
			nodeType = common.DirNode
		} else if fileMode&fs.ModeSymlink != 0 {
			_target, err := os.Readlink(currentPath)
			if err != nil {
				log.Warn().Err(err).Msgf("Error reading symlink target %s, skipping", currentPath)
				return nil
			}
			target = _target
			nodeType = common.SymLinkNode
		} else if fileMode.IsRegular() {
			nodeType = common.FileNode
		} else {
			log.Warn().Msgf("Skipping unsupported file type %s at %s", fileMode.String(), currentPath)
			return nil
		}

		var stat unix.Stat_t
		if nodeType == common.SymLinkNode {
			err = unix.Lstat(currentPath, &stat)
		} else {
			err = unix.Stat(currentPath, &stat)
		}
		if err != nil {
			log.Warn().Err(err).Msgf("Error stating path %s, skipping", currentPath)
			return nil
		}

		mode := uint32(stat.Mode & 0777)
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			mode |= syscall.S_IFDIR
		case unix.S_IFLNK:
			mode |= syscall.S_IFLNK
		case unix.S_IFREG:
			mode |= syscall.S_IFREG
		default:
			log.Error().Msgf("Unsupported file mode %v for path %s, skipping", stat.Mode&unix.S_IFMT, currentPath)
			return nil
		}

		var inode uint64
		if existingInode, exists := inodeMap[currentPath]; exists {
			inode = existingInode
		} else {
			inode = inodeGen.Next()
			inodeMap[currentPath] = inode
		}

		pathWithPrefix := filepath.Join("/", strings.TrimPrefix(currentPath, sourcePath))

		node := &common.ClipNode{
			Path:     pathWithPrefix,
			NodeType: nodeType,
			Attr: fuse.Attr{
				Ino:       inode,
				Size:      uint64(stat.Size),
				Blocks:    uint64(stat.Blocks),
				Atime:     uint64(stat.Atim.Sec),
				Atimensec: uint32(stat.Atim.Nsec),
				Mtime:     uint64(stat.Mtim.Sec),
				Mtimensec: uint32(stat.Mtim.Nsec),
				Ctime:     uint64(stat.Ctim.Sec),
				Ctimensec: uint32(stat.Ctim.Nsec),
				Mode:      mode,
				Nlink:     uint32(stat.Nlink),
				Owner: fuse.Owner{
					Uid: stat.Uid,
					Gid: stat.Gid,
				},
			},
			Target:  target,
			DataPos: -1,
			DataLen: -1,
		}
		index.Set(node)

		return nil
	})

	return err
}

type S3ChunkWriter interface {
	io.WriteCloser
	WaitForCompletion() error
}

// writePackedChunks writes file contents into chunks until the chunk has reached the max chunk size.
func (ca *ClipV2Archiver) writePackedChunks(index *btree.BTreeG[*common.ClipNode]) ([]string, error) {
	var (
		chunkNames []string
		offset     int64 = 0
		ctx              = context.Background()
		uploaders  []S3ChunkWriter
	)

	chunkNum := 0
	chunkName := fmt.Sprintf("%d%s", chunkNum, ChunkSuffix)
	chunkKey := fmt.Sprintf("%s/chunks/%s", ca.IndexID, chunkName)
	chunkWriter, err := ca.newChunkWriter(ctx, chunkKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create chunk writer: %w", err)
	}

	var filesToProcess []*common.ClipNode
	minNode, _ := index.Min()
	index.Ascend(minNode, func(node *common.ClipNode) bool {
		if node.NodeType == common.FileNode {
			filesToProcess = append(filesToProcess, node)
		}
		return true
	})

	spaceInBlock := ca.chunkSize
	fileReadBuffer := make([]byte, ca.chunkSize)
	for _, node := range filesToProcess {
		if node.Attr.Size == 0 {
			node.DataPos = offset
			node.DataLen = 0
			if ca.Verbose {
				log.Info().Msgf("Packing empty file: %s at offset %d", node.Path, offset)
			}
			continue
		}

		sourceFilePath := path.Join(ca.SourcePath, node.Path)
		f, err := os.Open(sourceFilePath)
		if err != nil {
			log.Error().Err(err).Msgf("Failed to open source file %s, skipping", node.Path)
			node.DataPos = -1
			node.DataLen = -1
			continue
		}

		if ca.Verbose {
			log.Info().Msgf("Packing file: %s (size %d) starting at offset %d", node.Path, node.Attr.Size, offset)
		}

		// Begin hash writer for the file
		hashWriter := sha256.New()
		writer := io.MultiWriter(chunkWriter, hashWriter)

		node.DataPos = offset
		node.DataLen = int64(node.Attr.Size)

		fileBytesProcessed := int64(0)
		for fileBytesProcessed < int64(node.Attr.Size) {
			if spaceInBlock <= 0 {
				// Finalize the current chunk
				if err := chunkWriter.Close(); err != nil {
					f.Close()
					return nil, fmt.Errorf("failed to close chunk writer: %w", err)
				}
				chunkNames = append(chunkNames, chunkName)
				if s3Writer, ok := chunkWriter.(*s3ChunkWriter); ok {
					uploaders = append(uploaders, s3Writer)
				}

				// Start a new chunk
				chunkNum++
				chunkName = fmt.Sprintf("%d%s", chunkNum, ChunkSuffix)
				chunkKey = fmt.Sprintf("%s/chunks/%s", ca.IndexID, chunkName)
				chunkWriter, err = ca.newChunkWriter(ctx, chunkKey)
				if err != nil {
					f.Close()
					return nil, fmt.Errorf("failed to create chunk writer: %w", err)
				}
				spaceInBlock = ca.chunkSize

				// New writer continues to write to the file's hashWriter
				writer = io.MultiWriter(chunkWriter, hashWriter)
			}

			// bytesToRead must be set to the minimum of
			// * remaining space in the current chunk
			// * remaining bytes in the file
			// * size of the read buffer
			bytesToRead := int64(len(fileReadBuffer))
			bytesToRead = min(bytesToRead, spaceInBlock)
			bytesRemainingInFile := int64(node.Attr.Size) - fileBytesProcessed
			bytesToRead = min(bytesToRead, bytesRemainingInFile)

			n, readErr := io.ReadFull(f, fileReadBuffer[:bytesToRead])

			if n > 0 {
				written, writeErr := writer.Write(fileReadBuffer[:n])
				if writeErr != nil {
					f.Close()
					return nil, fmt.Errorf("failed writing %d bytes for %s to block: %w", n, node.Path, writeErr)
				}
				if written != n {
					f.Close()
					return nil, fmt.Errorf("short write to block buffer (%d vs %d)", written, n)
				}

				offset += int64(written)
				fileBytesProcessed += int64(written)
				spaceInBlock -= int64(written)
			}

			if readErr != nil {
				if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
					break
				} else {
					log.Error().Err(readErr).Msgf("Error reading source file %s, marking incomplete", node.Path)
					node.DataPos = -1
					node.DataLen = -1
					f.Close()
					return nil, fmt.Errorf("error reading source file %s: %w", node.Path, readErr)
				}
			}
		}
		f.Close()

		if node.DataPos != -1 && fileBytesProcessed != int64(node.Attr.Size) {
			log.Warn().Msgf("File processing incomplete for %s: expected %d, processed %d", node.Path, node.Attr.Size, fileBytesProcessed)
			node.DataPos = -1
			node.DataLen = -1
		}

		// Finalize the hash
		node.ContentHash = hex.EncodeToString(hashWriter.Sum(nil))
	}

	if err := chunkWriter.Close(); err != nil {
		return nil, fmt.Errorf("failed to close chunk writer: %w", err)
	}
	chunkNames = append(chunkNames, chunkName)
	if s3Writer, ok := chunkWriter.(*s3ChunkWriter); ok {
		uploaders = append(uploaders, s3Writer)
	}

	// Wait for all uploads to complete
	for _, uploader := range uploaders {
		if err := uploader.WaitForCompletion(); err != nil {
			return nil, fmt.Errorf("failed to upload chunk: %w", err)
		}
	}

	log.Info().Msgf("Finished packing. Total chunks created: %d", len(chunkNames))
	return chunkNames, nil
}

// extractHeader extracts the header from the given file.
func (ca *ClipV2Archiver) extractHeader(file io.Reader) (*ClipV2ArchiveHeader, error) {
	header, err := ca.DecodeHeader(file)
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("index file is empty or truncated before header length: %w", common.ErrFileHeaderMismatch)
		}
		return nil, fmt.Errorf("failed to decode header: %w", err)
	}

	if !bytes.Equal(header.StartBytes[:], common.ClipFileStartBytes) || header.ClipFileFormatVersion != ClipV2FileFormatVersion {
		return nil, common.ErrFileHeaderMismatch
	}

	return header, nil
}

// extractChunkList extracts the chunk list from the given file.
func (ca *ClipV2Archiver) extractChunkList(file io.Reader, chunkListLength int64) ([]string, error) {
	if chunkListLength < 0 {
		return nil, fmt.Errorf("invalid negative chunk list length in header: %d", chunkListLength)
	}

	chunkListBytes := make([]byte, chunkListLength)
	if chunkListLength > 0 {
		if _, err := io.ReadFull(file, chunkListBytes); err != nil {
			return nil, fmt.Errorf("error reading chunk list data (length %d): %w", chunkListLength, err)
		}
	}

	var chunkList []string
	if err := gob.NewDecoder(bytes.NewReader(chunkListBytes)).Decode(&chunkList); err != nil {
		return nil, fmt.Errorf("error decoding chunk list: %w", err)
	}

	return chunkList, nil
}

// extractIndex extracts the index from the given file.
func (ca *ClipV2Archiver) extractIndex(header *ClipV2ArchiveHeader, file io.Reader) (*btree.BTreeG[*common.ClipNode], error) {
	indexBytes := make([]byte, header.IndexLength)
	if header.IndexLength < 0 {
		return nil, fmt.Errorf("invalid negative index length in header: %d", header.IndexLength)
	}
	if header.IndexLength > 0 {
		if _, err := io.ReadFull(file, indexBytes); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil, fmt.Errorf("index file is truncated (index data): %w", err)
			}
			return nil, fmt.Errorf("error reading index data (length %d): %w", header.IndexLength, err)
		}
	}

	var nodes []*common.ClipNode
	if len(indexBytes) > 0 {
		indexReader := bytes.NewReader(indexBytes)
		indexDec := gob.NewDecoder(indexReader)
		if err := indexDec.Decode(&nodes); err != nil {
			return nil, fmt.Errorf("error decoding index gob data: %w", err)
		}
	} else {
		nodes = make([]*common.ClipNode, 0)
	}

	index := newIndex()
	for _, node := range nodes {
		index.Set(node)
	}

	return index, nil
}

func (ca *ClipV2Archiver) newChunkWriter(ctx context.Context, chunkKey string) (S3ChunkWriter, error) {
	if ca.StorageType == common.StorageModeS3 {
		chunkWriter, err := newS3ChunkWriter(ctx, ca.S3Config, chunkKey)
		return chunkWriter, err
	}

	localChunkPath := filepath.Join(ca.LocalPath, "/"+chunkKey)
	dir := filepath.Dir(localChunkPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	chunkWriter, err := os.Create(filepath.Join(ca.LocalPath, chunkKey))
	return &LocalChunkWriter{chunkWriter}, err
}

func (ca *ClipV2Archiver) newIndexWriter(ctx context.Context) (S3ChunkWriter, error) {
	if ca.StorageType == common.StorageModeS3 {
		indexKey := fmt.Sprintf("%s/index.clip", ca.IndexID)
		return newS3ChunkWriter(ctx, ca.S3Config, indexKey)
	}

	indexFilePath := filepath.Join(ca.LocalPath, ca.IndexID, "index.clip")
	indexFile, err := os.Create(indexFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create index file %s: %w", indexFilePath, err)
	}

	return &LocalChunkWriter{indexFile}, nil
}

func newIndex() *btree.BTreeG[*common.ClipNode] {
	compare := func(a, b *common.ClipNode) bool {
		return a.Path < b.Path
	}
	return btree.NewBTreeGOptions(compare, btree.Options{NoLocks: false})
}

func (ca *ClipV2Archiver) newIndexReader(ctx context.Context) (io.ReadCloser, error) {
	var (
		archiveReader io.ReadCloser
		err           error
	)

	switch ca.StorageType {
	case common.StorageModeS3:
		// Get file from S3
		cfg, err := config.LoadDefaultConfig(ctx,
			config.WithRegion(ca.S3Config.Region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
				ca.S3Config.AccessKey,
				ca.S3Config.SecretKey,
				"",
			)),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}

		s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			if ca.S3Config.ForcePathStyle {
				o.UsePathStyle = true
			}
			o.BaseEndpoint = aws.String(ca.S3Config.Endpoint)
		})

		indexKey := fmt.Sprintf("%s/index.clip", ca.IndexID)
		obj, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(ca.S3Config.Bucket),
			Key:    aws.String(indexKey),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to get object from S3: %w", err)
		}

		archiveReader = obj.Body
	default:
		indexPath := filepath.Join(ca.LocalPath, ca.IndexID, "index.clip")
		archiveReader, err = os.Open(indexPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open index file %s: %w", indexPath, err)
		}
	}

	return archiveReader, nil
}
