/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package uploadcmd

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-cli/pkg/environment"

	"github.com/trustbloc/sidetree-core-go/pkg/docutil"
	"github.com/trustbloc/sidetree-core-go/pkg/restapi/helper"

	"github.com/trustbloc/fabric-cli-ext/cmd/basecmd"
	"github.com/trustbloc/fabric-cli-ext/cmd/file/httpclient"
	"github.com/trustbloc/fabric-cli-ext/cmd/file/model"
)

const (
	use      = "upload"
	desc     = "Upload a file to DCAS"
	longDesc = `
The upload command allows a client to upload one or more files to DCAS and add them to a Sidetree file index document. The response is a JSON document that contains the names of the files that were updated along with their DCAS ID and content-type.
`
	examples = `
- Upload two files to the '/content' path and add index entries to the given file index document:
    $ ./fabric file upload --url http://localhost:48326/content --files ./fixtures/testdata/v1/person.schema.json;./fixtures/testdata/v1/raised-hand.png --idxurl http://localhost:48326/file/file:idx:EiAuN66iEpuRt6IIu-2sO3bRM74sS_AIuY6jTbtFUsqAaA== --pwd pwd1 --nextpwd pwd2 --noprompt

	Response:
		[
		  {
			"Name": "person.schema.json",
			"ID": "TbVyraOqG00TacPQH5WwWGnxkszpYSEhBKRyX_f25JI=",
			"ContentType": "application/json"
		  },
		  {
			"Name": "raised-hand.png",
			"ID": "k1fqlkDdtmkTBVTHQgvpJbhTEch2XP0cn0C-DuP-9pE=",
			"ContentType": "image/png"
		  }
		]
`
)

const (
	fileFlag  = "files"
	fileUsage = "The semi-colin separated paths of the files to upload. Example: --files ./samples/content1.json;./samples/image.png"

	urlFlag  = "url"
	urlUsage = "The URL to which to add the file(s). Example: --url http://localhost:48326/content"

	fileIndexURLFlag  = "idxurl"
	fileIndexURLUsage = "The URL of the file index Sidetree document to be updated with the new/updated files. Example: --idxurl http://localhost:48326/file/file:idx:1234"

	fileIndexUpdateOTPFlag  = "pwd"
	fileIndexUpdateOTPUsage = "The one time password required to update the file index Sidetree document. Example: --pwd pwd1"

	fileIndexNextUpdateOTPFlag  = "nextpwd"
	fileIndexNextUpdateOTPUsage = "The one time password required for the next update of the file index Sidetree document. Example: --nextpwd pwd2"

	noPromptFlag  = "noprompt"
	noPromptUsage = "If specified then the upload operation will not prompt for confirmation. Example: --noprompt"

	msgAborted         = "Operation aborted"
	msgContinueOrAbort = "Enter Y to continue or N to abort "

	sha2_256 = 18

	jsonPatchBasePath  = "/fileIndex/mappings/"
	jsonPatchAddOp     = "add"
	jsonPatchReplaceOp = "replace"
)

var (
	errURLRequired                    = errors.New("URL (--url) is required")
	errFilesRequired                  = errors.New("files (--files) is required")
	errFileIndexURLRequired           = errors.New("file index URL (--idxurl) is required")
	errFileIndexUpdateOTPRequired     = errors.New("password (--pwd) required")
	errFileIndexNextUpdateOTPRequired = errors.New("next update password (--nextpwd) required")
	errNoFileExtension                = errors.New("content type cannot be deduced since no file extension provided")
	errUnknownExtension               = errors.New("content type cannot be deduced from extension")
)

type httpClient interface {
	Post(url string, req []byte) (*httpclient.HTTPResponse, error)
	Get(url string) (*httpclient.HTTPResponse, error)
}

// New returns the file upload sub-command
func New(settings *environment.Settings) *cobra.Command {
	return newCmd(settings, httpclient.New())
}

func newCmd(settings *environment.Settings, client httpClient) *cobra.Command {
	c := &command{
		Command: basecmd.New(settings, nil),
		client:  client,
	}

	cmd := &cobra.Command{
		Use:     use,
		Short:   desc,
		Long:    longDesc,
		Example: examples,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.validateAndProcessArgs(); err != nil {
				return err
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			return c.run()
		},
	}

	c.Settings = settings
	cmd.SetOutput(c.Settings.Streams.Out)
	cmd.SilenceUsage = true

	cmd.Flags().StringVar(&c.file, fileFlag, "", fileUsage)
	cmd.Flags().StringVar(&c.url, urlFlag, "", urlUsage)
	cmd.Flags().StringVar(&c.fileIndexURL, fileIndexURLFlag, "", fileIndexURLUsage)
	cmd.Flags().StringVar(&c.fileIndexUpdateOTP, fileIndexUpdateOTPFlag, "", fileIndexUpdateOTPUsage)
	cmd.Flags().StringVar(&c.fileIndexNextUpdateOTP, fileIndexNextUpdateOTPFlag, "", fileIndexNextUpdateOTPUsage)
	cmd.Flags().BoolVar(&c.noPrompt, noPromptFlag, false, noPromptUsage)

	return cmd
}

// command implements the update command
type command struct {
	*basecmd.Command
	client httpClient

	file                   string
	url                    string
	basePath               string
	fileIndexURL           string
	fileIndexBaseURL       string
	fileIndexUpdateOTP     string
	fileIndexNextUpdateOTP string
	noPrompt               bool
}

func (c *command) validateAndProcessArgs() error {
	if c.url == "" {
		return errURLRequired
	}

	u, err := url.Parse(c.url)
	if err != nil {
		return errors.WithMessagef(err, "invalid URL [%s]", c.url)
	}

	if u.Path == "" {
		return errors.New("invalid URL - no base path found")
	}

	c.basePath = u.Path

	if c.file == "" {
		return errFilesRequired
	}

	if c.fileIndexURL == "" {
		return errFileIndexURLRequired
	}

	pos := strings.LastIndex(c.fileIndexURL, "/")
	if pos == -1 {
		return errors.Errorf("invalid file index URL: [%s]", c.fileIndexURL)
	}

	if c.fileIndexUpdateOTP == "" {
		return errFileIndexUpdateOTPRequired
	}

	if c.fileIndexNextUpdateOTP == "" {
		return errFileIndexNextUpdateOTPRequired
	}

	c.fileIndexBaseURL = c.fileIndexURL[0:pos]

	return nil
}

func (c *command) run() error {
	fileIdx, err := c.getFileIndex()
	if err != nil {
		return err
	}

	f, err := c.getFiles()
	if err != nil {
		return err
	}

	if !c.noPrompt {
		confirmed, e := c.confirmUpload(c.url, f)
		if e != nil {
			return e
		}

		if !confirmed {
			return c.Fprintln(msgAborted)
		}
	}

	for _, file := range f {
		id, e := c.upload(c.url, file.ContentType, file.Content)
		if e != nil {
			return e
		}

		file.ID = id
	}

	err = c.updateIndexFile(fileIdx, f)
	if err != nil {
		return err
	}

	return c.Fprint(f.String())
}

// confirmUpload prompts the user for confirmation of the upload
func (c *command) confirmUpload(url string, files files) (bool, error) {
	prompt := fmt.Sprintf("Uploading the following files to [%s]\n%s\n%s", url, files, msgContinueOrAbort)

	err := c.Fprintln(prompt)
	if err != nil {
		return false, err
	}

	return strings.ToLower(c.Prompt()) == "y", nil
}

func (c *command) upload(url, contentType string, fileBytes []byte) (string, error) {
	req := &uploadFile{
		ContentType: contentType,
		Content:     fileBytes,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	resp, err := c.client.Post(url, reqBytes)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", errors.Errorf("status code %d: %s", resp.StatusCode, resp.ErrorMsg)
	}

	var fileID string
	err = json.Unmarshal(resp.Payload, &fileID)
	if err != nil {
		return "", err
	}

	return fileID, nil
}

func (c *command) updateIndexFile(fileIdx *model.FileIndex, files files) error {
	patch, err := getUpdatePatch(fileIdx, files)
	if err != nil {
		return err
	}

	req, err := c.getUpdateRequest(patch)
	if err != nil {
		return err
	}

	resp, err := c.client.Post(c.fileIndexBaseURL, req)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("error updating file index document. Status code %d: %s", resp.StatusCode, resp.ErrorMsg)
	}

	return err
}

func (c *command) getFiles() (files, error) {
	var f files
	for _, filePath := range strings.Split(c.file, ";") {
		fileInfo, err := getFileInfo(filePath)
		if err != nil {
			return nil, err
		}

		f = append(f, fileInfo)
	}

	return f, nil
}

func (c *command) getUpdateRequest(patch jsonpatch.Patch) ([]byte, error) {
	uniqueSuffix, err := getUniqueSuffix(c.fileIndexURL)
	if err != nil {
		return nil, err
	}

	return helper.NewUpdateRequest(&helper.UpdateRequestInfo{
		DidUniqueSuffix: uniqueSuffix,
		UpdateOTP:       docutil.EncodeToString([]byte(c.fileIndexUpdateOTP)),
		NextUpdateOTP:   docutil.EncodeToString([]byte(c.fileIndexNextUpdateOTP)),
		Patch:           patch,
		MultihashCode:   sha2_256,
	})
}

func (c *command) getFileIndex() (*model.FileIndex, error) {
	resp, err := c.client.Get(c.fileIndexURL)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, errors.Errorf("file index document [%s] not found", c.fileIndexURL)
		}

		return nil, errors.Errorf("error retrieving file index document [%s] status code %d: %s", c.fileIndexURL, resp.StatusCode, resp.ErrorMsg)
	}

	fileIdxDoc := &model.FileIndexDoc{}
	err = json.Unmarshal(resp.Payload, fileIdxDoc)
	if err != nil {
		return nil, err
	}

	// Validate that the base path is correct
	if fileIdxDoc.FileIndex.BasePath != c.basePath {
		return nil, errors.Errorf("base path of file index doc does not match the base path of the file: [%s] != [%s]", fileIdxDoc.FileIndex.BasePath, c.basePath)
	}

	return &fileIdxDoc.FileIndex, nil
}

func getFileInfo(path string) (*fileInfo, error) {
	var fileName string
	p := strings.LastIndex(path, "/")
	if p == -1 {
		fileName = path
	} else {
		fileName = path[p+1:]
	}

	contentType, err := contentTypeFromFileName(fileName)
	if err != nil {
		return nil, err
	}

	content, err := ioutil.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}

	return &fileInfo{
		Name:        fileName,
		Content:     content,
		ContentType: contentType,
	}, nil
}

func contentTypeFromFileName(fileName string) (string, error) {
	p := strings.LastIndex(fileName, ".")
	if p == -1 {
		return "", errNoFileExtension
	}

	contentType := mime.TypeByExtension(fileName[p:])
	if contentType == "" {
		return "", errUnknownExtension
	}

	return contentType, nil
}

func getUpdatePatch(fileIdx *model.FileIndex, files files) (jsonpatch.Patch, error) {
	var patch []jsonPatch
	for _, f := range files {
		p := jsonPatch{
			Path:  jsonPatchBasePath + f.Name,
			Value: f.ID,
		}

		if _, ok := fileIdx.Mappings[f.Name]; ok {
			p.Op = jsonPatchReplaceOp
		} else {
			p.Op = jsonPatchAddOp
		}

		patch = append(patch, p)
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}

	jsonPatch, err := jsonpatch.DecodePatch(patchBytes)
	if err != nil {
		return nil, err
	}

	return jsonPatch, nil
}

func getUniqueSuffix(id string) (string, error) {
	p := strings.LastIndex(id, ":")
	if p == -1 {
		return "", errors.Errorf("unique suffix not provided in URL [%s]", id)
	}

	return id[p+1:], nil
}
