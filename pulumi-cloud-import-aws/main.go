package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/pulumi/pulumi/pkg/v3/codegen/dotnet"
	pschema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/aws/aws-sdk-go/aws"
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
	IncrementalImportMode
	ReadMode
)

func main() {
	incremental := isIncremental()
	isImportMode := isImportMode()
	if incremental && !isImportMode {
		panic("--incremental can only be used with --import")
	}

	// pulumi read resource mode
	if !isImportMode {
		pulumi.Run(func(ctx *pulumi.Context) error {

			_, err := buildImportSpec(ctx, ReadMode)
			if err != nil {
				return err
			}

			return nil
		})
	} else {
		mode := ImportMode
		if incremental {
			mode = IncrementalImportMode
		}
		imports, err := buildImportSpec(nil, mode)
		if err != nil {
			panic(err)
		}
		fmt.Printf("Total resources: %d", len(imports.Resources))

		err = writeImportFile(imports)
		if err != nil {
			panic(err)
		}

		// only run bulk import if not in incremental mode
		if !incremental {
			err = callBulkPulumiImport()
			if err != nil {
				panic(err)
			}
		}
	}
}

var resourcesToSkip = map[string]bool{
	// 'Account is not registered as a publisher' error
	"aws-native:cloudformation:PublicTypeVersion": true,
	// error: resource 'AwsDataCatalog' does not exist
	"aws-native:athena:DataCatalog": true,
	// error: resource 'LOCKE' does not exist
	"aws-native:appflow:Connector": true,
	// name collison Duplicate resource URN 'efs:FileSystem::EFSFileSystemfs0dce0ba5'; try giving it a unique name
	"aws-native:efs:FileSystem": true,
	// FAILED: [RSLVR-00903] Cannot tag Auto Defined Rule.
	"aws-native:route53resolver:ResolverRule": true,
}

func buildImportSpec(ctx *pulumi.Context, mode Mode) (importFile, error) {
	pkgSpec, err := getAWSNativeSchema()
	if err != nil {
		panic(err)
	}

	csharpRaw := pkgSpec.Language["csharp"]
	csharpInfo := dotnet.CSharpPackageInfo{}
	if err := json.Unmarshal(csharpRaw, &csharpInfo); err != nil {
		panic(err)
	}

	imports := importFile{
		Resources: []importSpec{},
	}

	sess, err := session.NewSession()
	if err != nil {
		panic(err)
	}
	client := cloudcontrolapi.New(sess, aws.NewConfig().WithMaxRetries(1000))

	importChan := make(chan importSpec)
	var wg sync.WaitGroup

	chunks := 3
	pkgChunks := make([][]string, chunks)
	index := 0
	// split input ino N chunks
	for k := range pkgSpec.Resources {
		pkgChunks[index] = append(pkgChunks[index], k)
		index++
		index = index % chunks
	}

	for i := 0; i < len(pkgChunks); i++ {
		pkgs := pkgChunks[i]
		wg.Add(1)
		go func(pkgChunk []string) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("encountered error processing AWS resources: %v \n", r)
				}
			}()
			defer wg.Done()

			seen := map[string]bool{}
			for _, k := range pkgChunk {
				if _, ok := resourcesToSkip[k]; ok {
					continue
				}
				parts := strings.Split(k, ":")

				namespace, ok := csharpInfo.Namespaces[parts[1]]
				if !ok {
					namespace = parts[1]
				}
				cloudControlType := fmt.Sprintf("AWS::%s::%s", namespace, parts[2])

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
									Name: clearString(fmt.Sprintf("%s%s%s", namespace, parts[2], *r.Identifier)),
								}
								importChan <- resource
							}
						}
						return true
					})

				// just print out errors as info for now
				// as there are some resources that don't support ListResources
				// or have special auth requirements.
				// TODO -- handle rate limiting errors
				if err != nil {
					fmt.Println(err)
				}

			}
		}(pkgs)
	}

	go func() {
		wg.Wait()
		close(importChan)
	}()

loop:
	for {
		select {
		case resource, ok := <-importChan:
			if !ok {
				break loop
			}
			imports.Resources = append(imports.Resources, resource)

			if mode == IncrementalImportMode {
				// currently, just swallow import errors and keep going
				_ = callIncrementalPulumiImport(imports.Resources[len(imports.Resources)-1])
			} else if mode == ReadMode {
				var res pulumi.CustomResourceState
				// currently ignore errors
				_ = ctx.ReadResource(resource.Type, resource.Name, pulumi.ID(resource.ID), nil, &res)
			}
		}
	}

	return imports, nil
}

// download https://raw.githubusercontent.com/pulumi/pulumi-aws-native/master/provider/cmd/pulumi-resource-aws-native/schema.json
// and parse it into a pschema.PackageSpec
func getAWSNativeSchema() (*pschema.PackageSpec, error) {
	schemaURL := "https://raw.githubusercontent.com/pulumi/pulumi-aws-native/master/provider/cmd/pulumi-resource-aws-native/schema.json"

	resp, err := http.Get(schemaURL)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	var schema pschema.PackageSpec
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	respByte := buf.Bytes()
	if err := json.Unmarshal(respByte, &schema); err != nil {
		return nil, err
	}

	return &schema, nil
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

func callBulkPulumiImport() error {
	// run pulumi import
	cmd := exec.Command("pulumi", "import", "-p", "1", "-f", "import.json", "--yes")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func callIncrementalPulumiImport(imp importSpec) error {
	// run pulumi import
	cmd := exec.Command("pulumi", "import", "--yes", "--skip-preview", imp.Type, imp.Name, imp.ID)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// check for presence of --incremental flag
func isIncremental() bool {
	for _, arg := range os.Args {
		if arg == "--incremental" {
			return true
		}
	}
	return false
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

var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9 ]+`)

func clearString(str string) string {
	return nonAlphanumericRegex.ReplaceAllString(str, "")
}
