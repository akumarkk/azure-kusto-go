package queued

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/ingest/ingestoptions"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/utils"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

func TestFormatDiscovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  properties.DataFormat
	}{
		{".avro.zip", properties.AVRO},
		{".AVRO.GZ", properties.AVRO},
		{".csv", properties.CSV},
		{".json", properties.JSON},
		{".orc", properties.ORC},
		{".parquet", properties.Parquet},
		{".psv", properties.PSV},
		{".raw", properties.Raw},
		{".scsv", properties.SCSV},
		{".sohsv", properties.SOHSV},
		{".tsv", properties.TSV},
		{".txt", properties.TXT},
		{".whatever", properties.DFUnknown},
		{".w3clogfile", properties.W3CLogFile},
	}

	for _, test := range tests {
		test := test // capture
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()

			got := properties.DataFormatDiscovery(test.input)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestCompressionDiscovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  ingestoptions.CompressionType
	}{
		{"https://somehost.somedomain.com:8080/v1/somestuff/file.gz", ingestoptions.GZIP},
		{"https://somehost.somedomain.com:8080/v1/somestuff/file.zip", ingestoptions.ZIP},
		{"/path/to/a/file.gz", ingestoptions.GZIP},
		{"/path/to/a/file.zip", ingestoptions.ZIP},
		{"/path/to/a/file", ingestoptions.CTNone},
	}

	for _, test := range tests {
		test := test // capture
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()

			got := utils.CompressionDiscovery(test.input)
			assert.Equal(t, test.want, got)
		})
	}

}

type fakeBlobstore struct {
	out       *bytes.Buffer
	shouldErr bool
}

func (f *fakeBlobstore) uploadBlobStream(_ context.Context, reader io.Reader, _ *azblob.Client, _ string, _ string, _ *azblob.UploadStreamOptions) (azblob.UploadStreamResponse, error) {
	if f.shouldErr {
		return azblob.UploadStreamResponse{}, fmt.Errorf("error")
	}
	_, err := io.Copy(f.out, reader)
	return azblob.UploadStreamResponse{}, err
}

func (f *fakeBlobstore) uploadBlobFile(_ context.Context, fi *os.File, _ *azblob.Client, _ string, _ string, _ *azblob.UploadFileOptions) (azblob.UploadFileResponse, error) {
	if f.shouldErr {
		return azblob.UploadFileResponse{}, fmt.Errorf("error")
	}
	_, err := io.Copy(f.out, fi)
	return azblob.UploadFileResponse{}, err
}

func TestLocalToBlob(t *testing.T) {
	t.Parallel()

	content := "hello world"
	u := "https://account.windows.net"
	to, err := azblob.NewClientWithNoCredential(u, nil)
	if err != nil {
		panic(err)
	}

	f, err := os.OpenFile("test_file", os.O_CREATE+os.O_RDWR, 0770)
	if err != nil {
		panic(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(f.Name())
	})
	_, _ = f.Write([]byte(content))
	_ = f.Close()

	fgzip, err := os.OpenFile("test_file.gz", os.O_CREATE+os.O_RDWR, 0770)
	if err != nil {
		panic(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(fgzip.Name())
	})

	zw := gzip.NewWriter(fgzip)

	_, err = zw.Write([]byte(content))
	if err != nil {
		panic(err)
	}
	_ = zw.Close()

	_, err = os.ReadFile(f.Name())
	if err != nil {
		panic(err)
	}

	tests := []struct {
		desc      string
		from      string
		props     *properties.All
		err       bool
		uploadErr bool
		errOp     errors.Op
		errKind   errors.Kind
	}{
		{
			desc:    "Can't open file",
			err:     true,
			from:    "/path/does/not/exist",
			errOp:   errors.OpFileIngest,
			errKind: errors.KLocalFileSystem,
		},
		{
			desc:    "Can't stat the file",
			err:     true,
			errOp:   errors.OpFileIngest,
			errKind: errors.KLocalFileSystem,
		},
		{
			desc:      "Upload Stream fails",
			from:      f.Name(),
			err:       true,
			uploadErr: true,
			errOp:     errors.OpFileIngest,
			errKind:   errors.KBlobstore,
		},
		{
			desc:      "Upload file fails",
			from:      f.Name(),
			err:       true,
			uploadErr: true,
			errOp:     errors.OpFileIngest,
			errKind:   errors.KBlobstore,
		},
		{
			desc: "Stream success",
			from: f.Name(),
		},
		{
			desc: "File success",
			from: fgzip.Name(),
		},
	}

	for _, test := range tests {
		fbs := &fakeBlobstore{shouldErr: test.uploadErr, out: &bytes.Buffer{}}

		in := &Ingestion{
			db:           "database",
			table:        "table",
			uploadStream: fbs.uploadBlobStream,
			uploadBlob:   fbs.uploadBlobFile,
		}

		_, _, err := in.localToBlob(context.Background(), test.from, to, "test", &properties.All{})
		switch {
		case err == nil && test.err:
			t.Errorf("TestLocalToBlob(%s): got err == nil, want err != nil", test.desc)
			continue
		case err != nil && !test.err:
			t.Errorf("TestLocalToBlob(%s): got err == %s, want err == nil", test.desc, err)
			continue
		case err != nil:
			continue
		}

		gotBuf := &bytes.Buffer{}
		zr, err := gzip.NewReader(fbs.out)
		if err != nil {
			panic(err)
		}
		if _, err := io.Copy(gotBuf, zr); err != nil {
			t.Errorf("TestLocalToBlob(%s): on gzip decompress: err == %s", test.desc, err)
			continue
		}

		if gotBuf.String() != content {
			t.Errorf("TestLocalToBlob(%s): got %q, want %q", test.desc, gotBuf.String(), content)
		}
	}
}

type fileInfo struct {
	os.FileInfo
	isDir bool
}

func (f fileInfo) IsDir() bool {
	return f.isDir
}

func fakeStat(name string) (os.FileInfo, error) {
	switch name {
	case "c:\\dir\\file":
		return fileInfo{}, nil
	case "/mnt/dir/":
		return fileInfo{isDir: true}, nil
	}
	return nil, fmt.Errorf("error")
}

func TestIsLocalPath(t *testing.T) {
	statFunc = fakeStat
	t.Cleanup(func() {
		statFunc = os.Stat
	})

	tests := []struct {
		desc string
		path string
		err  bool
		want bool
	}{
		{
			desc: "error: valid path to local dir",
			path: "/mnt/dir",
			err:  true,
		},
		{
			desc: "error: invalid remote path ftp",
			path: "ftp://some.ftp.com",
			err:  true,
		},
		{
			desc: "success: valid http path",
			path: "http://some.http.com/path",
			want: false,
		},
		{
			desc: "success: valid https path",
			path: "https://some.https.com/path",
			want: false,
		},
		{
			desc: "success: valid path to local file",
			path: "c:\\dir\\file",
			want: true,
		},
	}

	for _, test := range tests {
		test := test // capture
		t.Run(test.desc, func(t *testing.T) {
			t.Parallel()

			got, err := IsLocalPath(test.path)

			if test.err {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			assert.Equal(t, test.want, got)
		})
	}
}

func TestShouldCompress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		props *properties.All
		want  bool
	}{
		{
			name: "Some file",
			props: &properties.All{Source: properties.SourceOptions{CompressionType: ingestoptions.CTUnknown,
				OriginalSource: "https://somehost.somedomain.com:8080/v1/somestuff/file"}},
			want: true,
		},
		{
			name: "Some file2",
			props: &properties.All{Source: properties.SourceOptions{CompressionType: ingestoptions.CTNone,
				OriginalSource: "https://somehost.somedomain.com:8080/v1/somestuff/file"}},
			want: true,
		},
		{
			name: "Provided compression type is GZIP",
			props: &properties.All{Source: properties.SourceOptions{CompressionType: ingestoptions.GZIP,
				OriginalSource: "https://somehost.somedomain.com:8080/v1/somestuff/file"}},
			want: false,
		},
		{
			name: "Guess by name is GZIP",
			props: &properties.All{Source: properties.SourceOptions{CompressionType: ingestoptions.CTUnknown,
				OriginalSource: "https://somehost.somedomain.com:8080/v1/somestuff/file.gz"}},
			want: false,
		},
		{
			name: "DontCompress is true",
			props: &properties.All{Source: properties.SourceOptions{CompressionType: ingestoptions.CTNone,
				DontCompress:   true,
				OriginalSource: "https://somehost.somedomain.com:8080/v1/somestuff/file"}},
			want: false,
		},
		{
			name: "Binary format",
			props: &properties.All{Source: properties.SourceOptions{CompressionType: ingestoptions.CTNone,
				OriginalSource: "https://somehost.somedomain.com:8080/v1/somestuff/file.avro"}},
			want: false,
		},
	}

	for _, test := range tests {
		test := test // capture
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			CompleteFormatFromFileName(test.props, test.props.Source.OriginalSource)

			got := ShouldCompress(test.props,
				utils.CompressionDiscovery(test.props.Source.OriginalSource))
			assert.Equal(t, test.want, got)
		})
	}
}
