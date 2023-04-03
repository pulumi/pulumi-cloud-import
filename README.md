# pulumi-insights-import-demo
Import infrastructure managed outside of Pulumi IaC into Pulumi Insights. Currently supports AWS only.

To run:

1. Clone this repo
3. `cd pulumi-insights-import`
2. Run `pulumi stack init <organization/aws-account-name>`
3. Set the appropriate AWS region you'd like to import resources from `export AWS_REGION=us-west-2`
4. Run the importer `go run main.go

TODO:
- Document how to hook this up to Pulumi Deployments + OIDC so this can be 1-click from the console
- Unfortunately this doesn't work end to end until https://github.com/pulumi/pulumi-aws-native/issues/854 is fixed. 
  In the meantime, I can call `pulumi import` one resource at a time which is very slow and frustrating...
