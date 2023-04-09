# pulumi-cloud-import
Import infrastructure managed outside of Pulumi IaC into Pulumi. Currently supports AWS and Azure.

## AWS

1. Clone this repo
3. `cd pulumi-cloud-import-aws`
2. Run `pulumi stack init <organization/aws-account-name-reginon>`
3. Set the appropriate AWS region you'd like to import resources from `export AWS_REGION=us-west-2`
4. pulumi up --skip-preview

## Azure

1. Clone this repo
3. `cd pulumi-cloud-import-azure`
2. Run `pulumi stack init <organization/azure-subscription-name-location>`
3. Set the appropriate Azure location you'd like to import resources from `export ARM_LOCATION=uswest2`
4. Set the Azure subscription you'd like to import from `export ARM_SUBSCRIPTION_ID=xxxxxxxxxxxxxxxxxxxxxxxxx`
4. Authenticate using your preferred method (i.e. `az login`)
4. pulumi up --skip-preview
