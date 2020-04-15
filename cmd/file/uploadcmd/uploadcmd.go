/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package uploadcmd

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-cli/pkg/environment"

	"github.com/trustbloc/sidetree-core-go/pkg/patch"
	"github.com/trustbloc/sidetree-core-go/pkg/restapi/helper"
	"github.com/trustbloc/sidetree-core-go/pkg/util/ecsigner"

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

	fileIndexUpdatePWDFlag  = "pwd"
	fileIndexUpdatePWDUsage = "The password required to update the file index Sidetree document. Example: --pwd pwd1"

	fileIndexNextUpdatePWDFlag  = "nextpwd"
	fileIndexNextUpdatePWDUsage = "The password required for the next update of the file index Sidetree document. Example: --nextpwd pwd2"

	fileIndexSigningKeyFlag  = "signingkey"
	fileIndexSigningKeyUsage = "The private key PEM used for signing the update of the index document. Example: --signingkey 'MHcCAQEEILmfa4yss8nsTJK2hKl+LAoiwW3p+eQzaHfITI9z8ptpoAoGCCqGSM49AwEHoUQDQgAEMd1/e/Nxh73bK12PEEcNSY9HxnP0N8er9ww9rjq1tNcsqfRjlL0bdTh9Basfn/4JrQHUHc6uS99yjQc+0u2bVg'"

	fileIndexSigningKeyFileFlag  = "signingkeyfile"
	fileIndexSigningKeyFileUsage = "The file that contains the private key PEM used for signing the update of the index document. Example: --signingkeyfile ./keys/signing.key"

	noPromptFlag  = "noprompt"
	noPromptUsage = "If specified then the upload operation will not prompt for confirmation. Example: --noprompt"

	msgAborted         = "Operation aborted"
	msgContinueOrAbort = "Enter Y to continue or N to abort "

	sha2_256         = 18
	signingAlgorithm = "ES256"

	jsonPatchBasePath  = "/fileIndex/mappings/"
	jsonPatchAddOp     = "add"
	jsonPatchReplaceOp = "replace"
)

var (
	errURLRequired                       = errors.New("URL (--url) is required")
	errFilesRequired                     = errors.New("files (--files) is required")
	errFileIndexURLRequired              = errors.New("file index URL (--idxurl) is required")
	errFileIndexUpdatePWDRequired        = errors.New("password (--pwd) required")
	errFileIndexNextUpdatePWDRequired    = errors.New("next update password (--nextpwd) required")
	errNoFileExtension                   = errors.New("content type cannot be deduced since no file extension provided")
	errUnknownExtension                  = errors.New("content type cannot be deduced from extension")
	errSigningKeyOrFileRequired          = errors.New("either signing key (--signingkey) or key file (--signingkeyfile) is required")
	errOnlyOneOfSigningKeyOrFileRequired = errors.New("only one of signing key (--signingkey) or key file (--signingkeyfile) may be specified")
	errPrivateKeyNotFoundInPEM           = errors.New("private key not found in PEM")
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
	cmd.Flags().StringVar(&c.fileIndexUpdatePWD, fileIndexUpdatePWDFlag, "", fileIndexUpdatePWDUsage)
	cmd.Flags().StringVar(&c.fileIndexNextUpdatePWD, fileIndexNextUpdatePWDFlag, "", fileIndexNextUpdatePWDUsage)
	cmd.Flags().StringVar(&c.fileIndexSigningKeyString, fileIndexSigningKeyFlag, "", fileIndexSigningKeyUsage)
	cmd.Flags().StringVar(&c.fileIndexSigningKeyFile, fileIndexSigningKeyFileFlag, "", fileIndexSigningKeyFileUsage)
	cmd.Flags().BoolVar(&c.noPrompt, noPromptFlag, false, noPromptUsage)

	return cmd
}

// command implements the update command
type command struct {
	*basecmd.Command
	client httpClient

	file                      string
	url                       string
	basePath                  string
	fileIndexURL              string
	fileIndexBaseURL          string
	fileIndexUpdatePWD        string
	fileIndexNextUpdatePWD    string
	fileIndexSigningKeyFile   string
	fileIndexSigningKeyString string
	noPrompt                  bool
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

	if c.fileIndexUpdatePWD == "" {
		return errFileIndexUpdatePWDRequired
	}

	if c.fileIndexNextUpdatePWD == "" {
		return errFileIndexNextUpdatePWDRequired
	}

	if err := c.validateSigningKey(); err != nil {
		return err
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

func (c *command) getUpdateRequest(patchStr string) ([]byte, error) {
	uniqueSuffix, err := getUniqueSuffix(c.fileIndexURL)
	if err != nil {
		return nil, err
	}

	updatePatch, err := patch.NewJSONPatch(patchStr)
	if err != nil {
		return nil, err
	}

	updateKeySigner, err := c.updateKeySigner()
	if err != nil {
		return nil, err
	}

	return helper.NewUpdateRequest(&helper.UpdateRequestInfo{
		DidUniqueSuffix:       uniqueSuffix,
		UpdateRevealValue:     []byte(c.fileIndexUpdatePWD),
		NextUpdateRevealValue: []byte(c.fileIndexNextUpdatePWD),
		Patch:                 updatePatch,
		MultihashCode:         sha2_256,
		Signer:                updateKeySigner,
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

func (c *command) updateKeySigner() (helper.Signer, error) {
	privateKey, err := c.signingPrivateKey()
	if err != nil {
		return nil, err
	}

	return ecsigner.New(privateKey, signingAlgorithm, model.UpdateKeyID), nil
}

func (c *command) signingPrivateKey() (*ecdsa.PrivateKey, error) {
	if c.fileIndexSigningKeyFile != "" {
		return privateKeyFromFile(c.fileIndexSigningKeyFile)
	}

	return privateKeyFromPEM([]byte(c.fileIndexSigningKeyString))
}

func (c *command) validateSigningKey() error {
	if c.fileIndexSigningKeyFile == "" && c.fileIndexSigningKeyString == "" {
		return errSigningKeyOrFileRequired
	}

	if c.fileIndexSigningKeyFile != "" && c.fileIndexSigningKeyString != "" {
		return errOnlyOneOfSigningKeyOrFileRequired
	}

	return nil
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

func getUpdatePatch(fileIdx *model.FileIndex, files files) (string, error) {
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
		return "", err
	}

	return string(patchBytes), nil
}

func getUniqueSuffix(id string) (string, error) {
	p := strings.LastIndex(id, ":")
	if p == -1 {
		return "", errors.Errorf("unique suffix not provided in URL [%s]", id)
	}

	return id[p+1:], nil
}

func privateKeyFromFile(file string) (*ecdsa.PrivateKey, error) {
	keyBytes, err := ioutil.ReadFile(filepath.Clean(file))
	if err != nil {
		return nil, err
	}

	return privateKeyFromPEM(keyBytes)
}

func privateKeyFromPEM(privateKeyPEM []byte) (*ecdsa.PrivateKey, error) {
	privBlock, _ := pem.Decode(privateKeyPEM)
	if privBlock == nil {
		return nil, errPrivateKeyNotFoundInPEM
	}

	privKey, err := x509.ParseECPrivateKey(privBlock.Bytes)
	if err != nil {
		return nil, err
	}

	return privKey, nil
}
