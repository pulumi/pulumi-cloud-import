# pulumi-insights-import-demo
Import infrastructure managed outside of Pulumi IaC into Pulumi Insights. Currently supports AWS only.

To run:

1. Clone this repo
3. `cd pulumi-insights-import`
2. Run `pulumi stack init <organization/aws-account-name>`
3. Set the appropriate AWS region you'd like to import resources from `export AWS_REGION=us-west-2`
4. pulumi up -p 5 --skip-preview

TODO:
- Document how to hook this up to Pulumi Deployments + OIDC so this can be 1-click from the console
- Unfortunately this is very very slow until https://github.com/pulumi/pulumi-aws-native/issues/854 is fixed

Now supports running as a pulumi program (`pulumi up`) to read resources via dynamic `ctx.ReadResource` operations. Still supports `import` mode via a separate entrypoint, so that we can extend this to import resources and generate code in the future (`go run main.go --import`).
