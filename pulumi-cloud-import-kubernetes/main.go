package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/pulumi/pulumi/pkg/v3/codegen/dotnet"
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
	ReadMode
)

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
	pkgSpec, err := getKubernetesNativeSchema()
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

	// Load kubeconfig file
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load kubeconfig: %v\n", err)
		os.Exit(1)
	}
	config.Burst = 120
	config.QPS = 50

	// Create Kubernetes clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create Kubernetes clientset: %v\n", err)
		os.Exit(1)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create dynamic client: %v\n", err)
		os.Exit(1)
	}

	// List API resources
	apiResources, err := clientset.Discovery().ServerPreferredResources()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list API resources: %v\n", err)
		os.Exit(1)
	}

	type res struct {
		Token string
		Name  string
		ID    string
	}
	resources := map[string]res{}

	token := func(x *unstructured.Unstructured) string {
		var gv string
		if x.GroupVersionKind().Group == "" {
			gv = fmt.Sprintf("core/%s", x.GroupVersionKind().Version)
		} else {
			gv = x.GroupVersionKind().GroupVersion().String()
		}
		return fmt.Sprintf("kubernetes:%s:%s", gv, x.GroupVersionKind().Kind)
	}
	id := func(x *unstructured.Unstructured) string {
		if x.GetNamespace() != "" {
			return fmt.Sprintf("%s/%s", x.GetNamespace(), x.GetName())
		}
		return x.GetName()
	}

	// TODO: may want to parallelize
	for _, group := range apiResources {
		for _, resource := range group.APIResources {
			gv, err := schema.ParseGroupVersion(group.GroupVersion)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to parse GroupVersion: %v\n", err)
				continue
			}
			gvr := gv.WithResource(resource.Name)
			obj, err := dynamicClient.Resource(gvr).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				// TODO: skip unsupported resource types
				//fmt.Fprintf(os.Stderr, "Failed to list objects for %s: %v\n", gvr.String(), err)
				continue
			}
			for _, i := range obj.Items {
				r := res{
					Token: token(&i),
					Name:  id(&i),
					ID:    id(&i),
				}

				resources[r.Token] = r
			}
		}
	}
	for _, r := range resources {
		if mode == ReadMode {
			var res pulumi.CustomResourceState
			// currently ignore errors
			_ = ctx.ReadResource(r.Token, r.Name, pulumi.ID(r.ID), nil, &res)
		}

	}

	return imports, nil
}

// download https://raw.githubusercontent.com/pulumi/pulumi-kubernetes/master/provider/cmd/pulumi-resource-kubernetes/schema.json
// and parse it into a pschema.PackageSpec
func getKubernetesNativeSchema() (*pschema.PackageSpec, error) {
	schemaURL := "https://raw.githubusercontent.com/pulumi/pulumi-kubernetes/master/provider/cmd/pulumi-resource-kubernetes/schema.json"

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
