package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/pulumi/pulumi/pkg/v3/codegen/dotnet"
	pschema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"

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

func main() {

	incremental := isIncremental()

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
	client := cloudcontrolapi.New(sess)
	totalResources := 0

	for k := range pkgSpec.Resources {
		parts := strings.Split(k, ":")

		namespace, ok := csharpInfo.Namespaces[parts[1]]
		if !ok {
			namespace = parts[1]
		}
		cloudControlType := fmt.Sprintf("AWS::%s::%s", namespace, parts[2])
		fmt.Println(cloudControlType)

		resourceNumber := 0
		params := &cloudcontrolapi.ListResourcesInput{
			MaxResults: aws.Int64(100),
			TypeName:   aws.String(cloudControlType),
		}
		err = client.ListResourcesPages(params,
			func(page *cloudcontrolapi.ListResourcesOutput, lastPage bool) bool {
				for _, r := range page.ResourceDescriptions {
					resourceNumber++
					totalResources++
					if r.Identifier != nil {
						imports.Resources = append(imports.Resources, importSpec{
							ID:   *r.Identifier,
							Type: k,
							Name: fmt.Sprintf("%s%s%d", namespace, parts[2], resourceNumber),
						})

						if incremental {
							// currently, just swallow import errors and keep going
							_ = callIncrementalPulumiImport(imports.Resources[len(imports.Resources)-1])
						}
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

	fmt.Printf("Total resources: %d", totalResources)

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
