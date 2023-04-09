package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/gertd/go-pluralize"
	pschema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
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

var resourcesToSkip = map[string]bool{}

func buildImportSpec(ctx *pulumi.Context, mode Mode) (importFile, error) {
	subscriptionID := getSubscriptionID()
	location := getLocation()

	pkgSpec, err := getAzureNativeSchema()
	if err != nil {
		panic(err)
	}

	imports := importFile{
		Resources: []importSpec{},
	}

	pluralize := pluralize.NewClient()

	importChan := make(chan importSpec)
	var wg sync.WaitGroup

	// right now this chunking is unnecessary, but in leaving the structure in place as in the future we might support
	// parallelizing input by resource group.
	chunks := 1

	for i := 0; i < chunks; i++ {
		wg.Add(1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("encountered error processing Azure resources: %v \n", r)
				}
			}()
			defer wg.Done()

			seen := map[string]bool{}

			cred, err := azidentity.NewDefaultAzureCredential(nil)
			if err != nil {
				panic(fmt.Sprintf("Authentication failure: %+v", err))
			}

			// Azure SDK Azure Resource Management clients accept the credential as a parameter
			client, _ := armresources.NewClient(subscriptionID, cred, nil)
			locationFilter := fmt.Sprintf("location eq '%s'", location)

			pager := client.NewListPager(&armresources.ClientListOptions{
				Filter: &locationFilter,
			})
			for pager.More() {
				page, err := pager.NextPage(context.Background())
				if err != nil {
					log.Fatalf("Failed to list resources: %+v", err)
				}

				for _, resource := range page.ResourceListResult.Value {
					id := *resource.ID
					parts := strings.Split(*resource.Type, ".")
					parts = strings.Split(parts[1], "/")
					nameParts := strings.Split(*resource.ID, "/")
					namespace := parts[0]
					resourceType := pluralize.Singular(strings.Title(parts[len(parts)-1]))
					name := nameParts[len(nameParts)-1]
					typeToken := fmt.Sprintf("azure-native:%s:%s", strings.ToLower(namespace), resourceType)

					if _, ok := pkgSpec.Resources[typeToken]; !ok {
						fmt.Printf("skipping resource %s because it is not in the schema, translated to %s (this could be a bug)\n", *resource.Type, typeToken)
						continue
					}

					if _, ok := resourcesToSkip[typeToken]; ok {
						continue
					}

					if seen[id] {
						continue
					}
					seen[id] = true

					resource := importSpec{
						ID:   id,
						Type: typeToken,
						Name: clearString(name),
					}
					importChan <- resource
				}
			}

		}()
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

// download hhttps://raw.githubusercontent.com/pulumi/pulumi-azure-native/master/provider/cmd/pulumi-resource-azure-native/schema.json
// and parse it into a pschema.PackageSpec
func getAzureNativeSchema() (*pschema.PackageSpec, error) {
	schemaURL := "https://raw.githubusercontent.com/pulumi/pulumi-azure-native/master/provider/cmd/pulumi-resource-azure-native/schema.json"

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

// reads ARM_LOCATION env var or returns default of uswest2
func getLocation() string {
	location := os.Getenv("ARM_LOCATION")
	if location == "" {
		location = "uswest2"
	}
	return location
}

// reads ARM_SUBSCRIPTION_ID env var or panics if none is set
func getSubscriptionID() string {
	subscriptionID := os.Getenv("ARM_SUBSCRIPTION_ID")
	if subscriptionID == "" {
		panic("ARM_SUBSCRIPTION_ID env var must be set")
	}
	return subscriptionID
}
