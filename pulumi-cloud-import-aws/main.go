package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudcontrolapi"
)

type importFile struct {
	NameTable map[string]resource.URN `json:"nameTable"`
	Resources []importSpec            `json:"resources"`
}

type importSpec struct {
	Type              string   `json:"type"`
	Name              string   `json:"name"`
	ID                string   `json:"id"`
	Parent            string   `json:"parent"`
	Provider          string   `json:"provider"`
	Version           string   `json:"version"`
	PluginDownloadURL string   `json:"pluginDownloadUrl"`
	Properties        []string `json:"properties"`
}

type Mode int64

const (
	ImportMode Mode = iota
	ReadMode
)

type CustomRetryer struct {
	client.DefaultRetryer
}

// We download metadata from pulumi-aws-native to get supported types.
// This sturct is only a subset of the full metadata.json
type cfType struct {
	CF string `json:"cf"`
}
type metadataResponse struct {
	Resources map[string]cfType `json:"resources"`
}

// ShouldRetry overrides the SDK's built in DefaultRetryer adding customization
// to not retry 500 internal server errors status codes.
// TODO: some AWS services consistently return 500 internal server errors
// when we hit the API. We shoudl open bugs against AWS for these.
func (r CustomRetryer) ShouldRetry(req *request.Request) bool {
	if req.HTTPResponse.StatusCode == 500 {
		// Don't retry any 500 status codes.
		return false
	}

	// Fallback to SDK's built in retry rules
	return r.DefaultRetryer.ShouldRetry(req)
}

func debugLog(a ...any) {
	if os.Getenv("PULUMI_CLOUD_IMPORT_DEBUG") != "" {
		fmt.Println(a...)
	}
}

func main() {
	isImportMode := isImportMode()

	// pulumi read resource mode
	if !isImportMode {
		pulumi.Run(func(ctx *pulumi.Context) error {
			_, err := buildImportSpec(ctx, ReadMode)
			return err
		})
	} else {
		mode := ImportMode
		imports, err := buildImportSpec(nil, mode)
		if err != nil {
			panic(err)
		}
		fmt.Printf("Total resources: %d", len(imports.Resources))

		err = writeImportFile(imports)
		if err != nil {
			panic(err)
		}
	}
}

func buildImportSpec(ctx *pulumi.Context, mode Mode) (importFile, error) {

	awsNativeTypesMap, err := getAWSNativeMetadata()
	if err != nil {
		panic(err)
	}

	imports := importFile{
		Resources: []importSpec{},
	}

	r := CustomRetryer{
		DefaultRetryer: client.DefaultRetryer{
			NumMaxRetries: 1000,
		},
	}
	c := aws.NewConfig()
	c.Retryer = r
	if os.Getenv("PULUMI_CLOUD_IMPORT_DEBUG") != "" {
		c.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	sess, err := session.NewSession(c)
	if err != nil {
		panic(err)
	}

	var ops uint64

	importChan := make(chan importSpec, 100000)
	var wg sync.WaitGroup

	chunks := getConcurrentWorkers()
	pkgChunks := make([][]string, chunks)
	index := 0
	// split input ino N chunks
	for k := range *awsNativeTypesMap {
		pkgChunks[index] = append(pkgChunks[index], k)
		index++
		index = index % chunks
	}

	for i := 0; i < chunks; i++ {
		pkgs := pkgChunks[i]
		wg.Add(1)
		go func(pkgChunk []string, i int) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("encountered error processing AWS resources: %v \n", r)
				}
			}()
			defer wg.Done()

			// AWS clients are not safe for concurrent use by multiple goroutines.
			client := cloudcontrolapi.New(sess)

			seen := map[string]bool{}
			for _, k := range pkgChunk {
				if _, ok := unsupportedResources[k]; ok {
					continue
				}
				cloudControlType, ok := (*awsNativeTypesMap)[k]
				if !ok {
					fmt.Println("Type definition not found - skipping", k)
					// This shouldn't happen
					continue
				}
				parts := strings.Split(cloudControlType, "::")
				params := &cloudcontrolapi.ListResourcesInput{
					MaxResults: aws.Int64(100),
					TypeName:   aws.String(cloudControlType),
				}
				err = client.ListResourcesPages(params,
					func(page *cloudcontrolapi.ListResourcesOutput, lastPage bool) bool {
						for _, r := range page.ResourceDescriptions {
							key := clearString(*r.Identifier)
							if seen[key] {
								continue
							}
							seen[key] = true
							if r.Identifier != nil {
								resource := importSpec{
									ID:   *r.Identifier,
									Type: k,
									// eg. name it S3Bucket<bucketName>
									Name: clearString(fmt.Sprintf("%s%s%s", parts[1], parts[2], *r.Identifier)),
								}
								atomic.AddUint64(&ops, 1)
								debugLog("worker:", i+1, "count:", atomic.LoadUint64(&ops))
								importChan <- resource
							}
						}
						return false
					})

				// just print out errors as info for now
				// as there are some resources that don't support ListResources
				// or have special auth requirements.
				if err != nil {
					fmt.Println("Failed to list resources of type", k, err)
				}
			}
			fmt.Printf("worker %d of %d completed\n", i+1, chunks)
		}(pkgs, i)
	}

	go func() {
		wg.Wait()
		close(importChan)
	}()

	for resource := range importChan {
		imports.Resources = append(imports.Resources, resource)
		if mode == ReadMode {
			var res pulumi.CustomResourceState
			// currently ignore errors
			_ = ctx.ReadResource(resource.Type, resource.Name, pulumi.ID(resource.ID), nil, &res)
		}

	}

	return imports, nil
}

// download https://raw.githubusercontent.com/pulumi/pulumi-aws-native/master/provider/cmd/pulumi-resource-aws-native/metadata.json
// and parse it into a metadataResponse struct
func getAWSNativeMetadata() (*map[string]string, error) {
	metadataURL := "https://raw.githubusercontent.com/pulumi/pulumi-aws-native/master/provider/cmd/pulumi-resource-aws-native/metadata.json"

	resp, err := http.Get(metadataURL)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	var schema metadataResponse
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	respByte := buf.Bytes()
	if err := json.Unmarshal(respByte, &schema); err != nil {
		return nil, err
	}

	// map from pulumi-aws-native type to cloudformation type
	typeMap := map[string]string{}
	for k, v := range schema.Resources {
		typeMap[k] = v.CF
	}

	return &typeMap, nil
}

// write import file to disk
func writeImportFile(imports importFile) error {
	// write the import file to disk
	importFile, err := json.MarshalIndent(imports, "", "    ")
	if err != nil {
		return err
	}

	err = ioutil.WriteFile("import.json", importFile, 0644)
	if err != nil {
		return err
	}

	return nil
}

// check for presence of --import flag
func isImportMode() bool {
	for _, arg := range os.Args {
		if arg == "--import" {
			return true
		}
	}
	return false
}

// getConcurrentWorkers the number of workers specified in PULUMI_CLOUD_IMPORT_WORKERS or returns a default of 3
func getConcurrentWorkers() int {
	workers, err := strconv.Atoi(os.Getenv("PULUMI_CLOUD_IMPORT_WORKERS"))
	if err != nil {
		return 10
	}
	return workers
}

var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9 ]+`)

func clearString(str string) string {
	return nonAlphanumericRegex.ReplaceAllString(str, "")
}
