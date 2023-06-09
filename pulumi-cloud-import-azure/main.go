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
	"regexp"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/gertd/go-pluralize"
	"github.com/hashicorp/go-azure-sdk/sdk/auth"
	"github.com/hashicorp/go-azure-sdk/sdk/environments"
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

type tokenWrapper struct {
	auth.Authorizer
}

func (t tokenWrapper) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	tok, err := t.Token(ctx, nil)
	if err != nil {
		panic(err)
	}
	at := azcore.AccessToken{
		Token:     tok.AccessToken,
		ExpiresOn: tok.Expiry,
	}

	return at, nil
}

var resourcesToSkip = map[string]bool{}

func buildImportSpec(ctx *pulumi.Context, mode Mode) (importFile, error) {
	imports := importFile{
		Resources: []importSpec{},
	}

	subscriptionID := getSubscriptionID()
	location := getLocation()

	pkgSpec, err := getAzureNativeSchema()
	if err != nil {
		panic(err)
	}

	pluralize := pluralize.NewClient()

	var wg sync.WaitGroup

	oidcToken := getOidcToken()

	var cred azcore.TokenCredential

	if oidcToken != "" {
		env := *environments.AzurePublic()
		c, err := auth.NewOIDCAuthorizer(context.Background(), auth.OIDCAuthorizerOptions{
			FederatedAssertion: oidcToken,
			TenantId:           getTenantID(),
			ClientId:           getClientID(),
			Environment:        env,
			Api:                env.ResourceManager,
		})
		if err != nil {
			panic(err)
		}

		cred = tokenWrapper{c}
	} else {
		cred, err = azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			panic(fmt.Sprintf("Authentication failure: %+v", err))
		}
	}

	// Azure SDK Azure Resource Management clients accept the credential as a parameter
	resourceClient, err := armresources.NewClient(subscriptionID, cred, nil)
	if err != nil {
		panic(err)
	}
	resourceGroupClient, err := armresources.NewResourceGroupsClient(subscriptionID, cred, nil)
	if err != nil {
		panic(err)
	}

	rgPager := resourceGroupClient.NewListPager(nil)

	resourceGroups := []importSpec{}

	for rgPager.More() {
		page, err := rgPager.NextPage(context.Background())
		if err != nil {
			log.Fatalf("Failed to list resources: %+v", err)
		}

		for _, resource := range page.ResourceGroupListResult.Value {
			if resource.Location != nil && *resource.Location != location {
				continue
			}
			id := *resource.ID
			name := *resource.Name
			resource := importSpec{
				ID:   id,
				Type: "azure-native:resources:ResourceGroup",
				Name: clearString(name),
			}
			resourceGroups = append(resourceGroups, resource)
		}
	}

	// create a buffered channel. we want to register all resource groups first, and then process resources so that parents are present
	importChan := make(chan importSpec, len(resourceGroups))

	for _, resourceGroup := range resourceGroups {
		importChan <- resourceGroup
	}

	// currently one goroutine per resource group. This could be too many for large subscriptions.
	chunks := len(resourceGroups)

	for i := 0; i < chunks; i++ {
		wg.Add(1)
		go func(resourceGroup string) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("encountered error processing Azure resources: %v \n", r)
				}
			}()
			defer wg.Done()

			seen := map[string]bool{}

			filter := fmt.Sprintf("location eq '%s'", location)

			rgParts := strings.Split(resourceGroup, "/")
			rgName := rgParts[len(rgParts)-1]

			pager := resourceClient.NewListByResourceGroupPager(rgName, &armresources.ClientListByResourceGroupOptions{
				Filter: &filter,
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
						ID:     id,
						Type:   typeToken,
						Name:   clearString(name),
						Parent: resourceGroup,
					}
					importChan <- resource
				}
			}

		}(resourceGroups[i].ID)
	}

	go func() {
		wg.Wait()
		close(importChan)
	}()

	rgs := map[string]pulumi.Resource{}

	for resource := range importChan {
		// create a new import spec as the parent needs to be a URN, so just strip it our for now
		imports.Resources = append(imports.Resources, importSpec{
			ID:   resource.ID,
			Type: resource.Type,
			Name: resource.Name,
		})
		if mode == ReadMode {
			var res pulumi.CustomResourceState
			// currently ignore errors
			if resource.Type == "azure-native:resources:ResourceGroup" {
				rgs[resource.ID] = &res
			}
			opts := []pulumi.ResourceOption{}
			if p, ok := rgs[resource.Parent]; ok {
				opts = append(opts, pulumi.Parent(p))
			}
			_ = ctx.ReadResource(resource.Type, resource.Name, pulumi.ID(resource.ID), nil, &res, opts...)
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
		location = "westus2"
	}
	return location
}

// reads ARM_SUBSCRIPTION_ID env var or ARM_SUBSCRIPTION_ID env var or panics if none is set
func getSubscriptionID() string {
	subscriptionID := os.Getenv("ARM_SUBSCRIPTION_ID")
	if subscriptionID == "" {
		subscriptionID = os.Getenv("AZURE_SUBSCRIPTION_ID")
	}
	if subscriptionID == "" {
		panic("ARM_SUBSCRIPTION_ID env var must be set")
	}
	return subscriptionID
}

// reads ARM_OIDC_TOKEN env var or AZURE_OIDC_TOKEN env var returns "" if none is set
func getOidcToken() string {
	token := os.Getenv("ARM_OIDC_TOKEN")
	if token == "" {
		token = os.Getenv("AZURE_OIDC_TOKEN")
	}
	return token
}

// reads ARM_CLIENT_ID env var or AZURE_CLIENT_ID env var or returns "" if none is set
func getClientID() string {
	clientID := os.Getenv("ARM_CLIENT_ID")
	if clientID == "" {
		clientID = os.Getenv("AZURE_CLIENT_ID")
	}
	return clientID
}

// reads ARM_TENANT_ID env var or AZURE_TENANT_ID env var or returns "" if none is set
func getTenantID() string {
	tenantID := os.Getenv("ARM_TENANT_ID")
	if tenantID == "" {
		tenantID = os.Getenv("AZURE_TENANT_ID")
	}
	return tenantID
}
