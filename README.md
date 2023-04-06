# pulumi-cloud-import
Import infrastructure managed outside of Pulumi IaC into Pulumi. Currently supports AWS only.

To run:

1. Clone this repo
3. `cd pulumi-cloud-import`
2. Run `pulumi stack init <organization/aws-account-name>`
3. Set the appropriate AWS region you'd like to import resources from `export AWS_REGION=us-west-2`
4. pulumi up --skip-preview

