# pulumi-cloud-import
Pulumi Cloud Import is an open source project that uses [Pulumi](github.com/pulumi/pulumi) to read all of the resources within your cloud account. There is a dedicated Pulumi program for each supported cloud provider. If you'd like to see more clouds supported please [open an issue on GitHub](https://github.com/pulumi/pulumi-cloud-import/issues/new).

Each import program for a given cloud provider follows the same general structure:

1. Use the cloud provider's REST API to list and iterate through all resources in the given account and region/location.
2. For each resource:
    1. Map the cloud provider type to a Pulumi provider type token.
    2. Call the pulumi method `ReadResource` with the resource's ID and provider type token. This loads the appropriate Pulumi provider that can read the full resource, all of it's metadata, encrypt all sensitive portions of the resource automatically, and store the result in the Pulumi state file.

Cloud Import programs use `ReadResource` which marks the resource as `external`, meaning the recource's lifecycle will not be managed by Pulumi and it will not be destroyed when you run `pulumi destroy`. `external` resources are discarded from the state file when you run `pulumi destroy` and no changes are made to the underlying resource or cloud account. As a result of this architecture, Cloud Import programs can be run with read-only credentials.

Since these are just normal Pulumi programs, you can configure and run them on your own including with your own backends.

Cloud Import programs are written in Go and require and Go 1.19+ to be installed on your system in addition the the Pulumi CLI.

### AWS

If you've never used Pulumi with AWS before we recommend you start first with the [Get Started with AWS](https://www.pulumi.com/docs/get-started/aws/) guide that helps you configure credentials and install dependencies.

The AWS Cloud Import program requires the Pulumi `aws-native` plugin with version >= `v0.57.0`. We recommend reinstalling aws-native plugins to be sure you pick up the latest version:

```console
$ pulumi plugin rm resource aws-native # delete older version of the plugin
$ pulumi plugin install resource aws-native # install the latest version
```

Once you have your environemnt configured with AWS credentials, run the following to get started:

```console
$ git clone https://github.com/pulumi/pulumi-cloud-import.git # cd into the cloned repo
$ cd pulumi-cloud-import-aws
$ pulumi stack init <org/stackname> # we recommend naming stacks accountName-regionName i.e. testing-us-west-2
$ pulumi config set aws-native:maxRetries 25 # cloud control API has aggressive rate limiting, so we need to retry with exponential backoff
$ export AWS_REGION=us-west-2 # the region of your AWS account
$ pulumi up --skip-preview --show-reads # run the aws cloud import program
```

The program uses 3 concurrent workers by default due to rate limits on the AWS cloud control API. You can control the concurrency through an environment variable: `PULUMI_CLOUD_IMPORT_WORKERS=10`.

### Azure

If you've never used Pulumi with Azure before we recommend you start first with the [Get Started with Azure](https://www.pulumi.com/docs/get-started/azure/) guide that helps you configure credentials and install dependencies.

Once you have your environemnt configured with Azure credentials, run the following to get started:

```console
$ git clone https://github.com/pulumi/pulumi-cloud-import.git # cd into the cloned repo
$ cd pulumi-cloud-import-azure
$ pulumi stack init <org/stackname> # we recommend naming stacks subscriptionName-location i.e. testing-westus2
$ export ARM_LOCATION=westus2 # the region of your Azure Subscription that you'd like to read from
$ export ARM_SUBSCRIPTION_ID=xxxxxxxxxxxxxxxxxxxxxxxxxx # ID of the Subscription that you'd like to read from
$ pulumi up --skip-preview --show-reads # run the azure cloud import program
```

### Debugging

Some programs provide additional debug logging. You can turn it on by setting `PULUMI_CLOUD_IMPORT_DEBUG=true` before running the program.

## Pulumi Cloud

Cloud Import is available as a fully managed experience within the Pulumi Cloud. The feature is currently in private preview and you can request access via [the waitlist](pulumi.com/product/private-previews). Once you have access, you can click on the `Cloud Import` tab to get started.

This managed experience uses [Pulumi Deployments](https://www.pulumi.com/docs/intro/deployments/) to run the Cloud Import program on your behalf. Within the Pulumi Cloud you can fill out a simple form that takes in your [OIDC configuration](https://www.pulumi.com/docs/intro/deployments/oidc/) to use a secure temporary credential workflow to connect to your cloud account. This will create the following:

## Generating Pulumi Code

Cloud Import can also generate Pulumi IaC code for you if you would like to manage the lifecycle of these resources with Pulumi IaC. This can be useful if you are looking for a way to clean up and delete a bunch of resources in bulk, or are looking to migrate a project off of ClickOps (or another tool) and onto IaC. Generating code uses the [`pulumi import`](https://www.pulumi.com/docs/reference/cli/pulumi_import/) CLI command. Running import will bring the lifecycle of your resources under the management of Pulumi. After running `pulumi import` running `pulumi up` may make changes to your resources in the backing cloud provider, and running `pulumi destroy` will delete them permantently. We will need to do a few things:

1. Generate an import file, which is a description of all resources, their types, and their identifiers.
2. Create a new Pulumi project and stack to house our resources and the code we're going to generate
3. Run `pulumi import` pointing to the import file we created

At the end of this, we'll have a new pulumi program with code that describes the resources we have deployed in the cloud, and a stack that contains the state and manages the lifecycle of all of those resources.

### Generating the Import File

Every Cloud Import program supports running in "import" mode. In this mode, it will not run a Pulumi program and will instead just crawl the cloud provider's list resources API and output an import file in the current working directory.

```console
$ git clone https://github.com/pulumi/pulumi-cloud-import.git # cd into the cloned repo
$ cd pulumi-cloud-import-aws # or cd into another cloud import program's repo
$ go run main.go --import # run the program in import mode, this does not require a Pulumi stack or CLI
$ ls # see that an `import.json` file has been created
```

Make note of the location of the `import.json` file, you will need it to run `pulumi import`

### Creating a New Project

In an empty directory create a new Pulumi program in your language of choice. You can do this by running one of the following `pulumi new` templates that will walk you through setting up a project and stack in your language of choice:

- `pulumi new typescript`
- `pulumi new python`
- `pulumi new csharp`
- `pulumi new go`
- `pulumi new yaml`

At this point, you will need to authenticate with your cloud provider of choice. See guides on cloud authentication in [AWS](https://www.pulumi.com/docs/get-started/aws/) and [Azure](https://www.pulumi.com/docs/get-started/azure/) for more details.

### Running `pulumi import`

From within your newly created Pulumi project, you'll now need to run pulumi import. This will import your resources into the Pulumi state file, and generate the code necessary to manage them. Run the appropriate import command given the language of your project:

- `pulumi import --file ./path-to-your/import.json --out index.ts`
- `pulumi import --file ./path-to-your/import.json --out __main__.py`
- `pulumi import --file ./path-to-your/import.json --out Program.cs`
- `pulumi import --file ./path-to-your/import.json --out main.go`

You can now inspect your newly generated program, make modifications, and run Pulumi operations such as `pulumi preview` or `pulumi refresh`.
