package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type importFile struct {
	NameTable map[string]resource.URN `json:"nameTable"`
	Resources []importSpec            `json:"resources"`
}

type importSpec struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	ID    string `json:"id"`
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
	start := time.Now()
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

	var ops uint64

	importChan := make(chan importSpec, 100000)
	var wg sync.WaitGroup

	chunks := getConcurrentWorkers()
	pkgChunks := make([][]*metav1.APIResourceList, chunks)
	index := 0
	// split resource groups into N chunks
	for _, group := range apiResources {
		pkgChunks[index] = append(pkgChunks[index], group)
		index++
		index = index % chunks
	}

	setupTime := time.Since(start)
	debugLog(fmt.Sprintf("Initialization time: %s\n", setupTime))

	for i := 0; i < chunks; i++ {
		pkgs := pkgChunks[i]
		wg.Add(1)
		go func(pkgChunk []*metav1.APIResourceList, i int) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("encountered error processing AWS resources: %v \n", r)
				}
			}()
			defer wg.Done()

			start := time.Now()
			for _, group := range pkgChunk {
				for _, res := range group.APIResources {
					gv, err := schema.ParseGroupVersion(group.GroupVersion)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Failed to parse GroupVersion: %v\n", err)
						continue
					}
					gvr := gv.WithResource(res.Name)
					obj, err := dynamicClient.Resource(gvr).List(context.Background(), metav1.ListOptions{})
					if err != nil {
						// TODO: skip unsupported resource types
						//fmt.Fprintf(os.Stderr, "Failed to list objects for %s: %v\n", gvr.String(), err)
						continue
					}
					for _, item := range obj.Items {
						r := importSpec{
							Token: token(&item),
							Name:  id(&item),
							ID:    id(&item),
						}

						atomic.AddUint64(&ops, 1)
						importChan <- r
					}
				}
			}
			stop := time.Since(start)
			debugLog("worker:", i+1, "count:", atomic.LoadUint64(&ops), "read time:", stop)
			fmt.Printf("worker %d of %d completed\n", i+1, chunks)
		}(pkgs, i)
	}

	go func() {
		wg.Wait()
		close(importChan)
	}()

	for r := range importChan {
		imports.Resources = append(imports.Resources, r)
		if mode == ReadMode {
			var res pulumi.CustomResourceState
			// currently ignore errors
			_ = ctx.ReadResource(r.Token, r.Name, pulumi.ID(r.ID), nil, &res)
		}

	}

	return imports, nil
}

// write import file to disk
func writeImportFile(imports importFile) error {
	// write the import file to disk
	importFile, err := json.MarshalIndent(imports, "", "    ")
	if err != nil {
		return err
	}

	err = os.WriteFile("import.json", importFile, 0644)
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
