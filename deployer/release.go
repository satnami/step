package deployer

import (
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/sfn"
	"github.com/coinbase/step/aws"
	"github.com/coinbase/step/aws/s3"
	"github.com/coinbase/step/machine"
	"github.com/coinbase/step/utils/is"
	"github.com/coinbase/step/utils/to"
)

type ReleaseError struct {
	Error *string
	Cause *string
}

// Release is the Data Structure passed between Client and Deployer
type Release struct {
	AwsAccountID *string `json:"aws_account_id,omitempty"`
	AwsRegion    *string `json:"aws_region,omitempty"`

	UUID      *string `json:"uuid,omitempty"`       // Generated By server
	ReleaseId *string `json:"release_id,omitempty"` // Generated Client

	ProjectName *string `json:"project_name,omitempty"`
	ConfigName  *string `json:"config_name,omitempty"`
	Bucket      *string `json:"bucket,omitempty"` // Bucket with Additional Data in it

	CreatedAt *time.Time `json:"created_at,omitempty"`

	// Deploy Releases
	LambdaName    *string `json:"lambda_name,omitempty"`   // Lambda Name
	LambdaSHA256  *string `json:"lambda_sha256,omitempty"` // Lambda SHA256 Zip file
	StepFnName    *string `json:"step_fn_name,omitempty"`  // Step Function Name
	ReleaseSHA256 string  `json:"release_sha256"`          // Not Set By Client

	StateMachineJSON *string `json:"state_machine_json,omitempty"`

	Error *ReleaseError `json:"error,omitempty"`

	Success *bool `json:"success,omitempty"`
}

//////////
// Validations
//////////

func (r *Release) ValidateAttributes() error {
	if is.EmptyStr(r.AwsAccountID) {
		return fmt.Errorf("AwsAccountID must be defined")
	}

	if is.EmptyStr(r.AwsRegion) {
		return fmt.Errorf("AwsRegion must be defined")
	}

	if is.EmptyStr(r.UUID) {
		return fmt.Errorf("UUID must be defined")
	}

	if err := r.ValidateClientAttributes(); err != nil {
		return err
	}

	if err := r.deployLambdaInput(to.ABytep([]byte{})).Validate(); err != nil {
		return err
	}

	if err := r.deployStepFunctionInput().Validate(); err != nil {
		return err
	}

	return nil
}

func (r *Release) ValidateClientAttributes() error {
	if is.EmptyStr(r.ReleaseId) {
		return fmt.Errorf("ReleaseId must be defined")
	}

	if is.EmptyStr(r.ProjectName) {
		return fmt.Errorf("ProjectName must be defined")
	}

	if is.EmptyStr(r.ConfigName) {
		return fmt.Errorf("ConfigName must be defined")
	}

	if is.EmptyStr(r.Bucket) {
		return fmt.Errorf("Bucket must be defined")
	}

	if r.CreatedAt == nil {
		return fmt.Errorf("CreatedAt must be defined")
	}

	// Created at date must be after 5 mins ago, and before 2 mins from now (wiggle room)
	if !is.WithinTimeFrame(r.CreatedAt, 300*time.Second, 120*time.Second) {
		return fmt.Errorf("Created at older than 5 mins (or in the future)")
	}

	if is.EmptyStr(r.LambdaName) {
		return fmt.Errorf("LambdaName must be defined")
	}

	if is.EmptyStr(r.LambdaSHA256) {
		return fmt.Errorf("LambdaSHA256 must be defined")
	}

	if is.EmptyStr(r.StepFnName) {
		return fmt.Errorf("StepFnName must be defined")
	}

	if is.EmptyStr(r.StateMachineJSON) {
		return fmt.Errorf("StateMachineJSON must be defined")
	}

	// Validate State machine
	if err := machine.Validate(r.StateMachineJSON); err != nil {
		return fmt.Errorf("StateMachineJSON invalid with '%v'", err.Error())
	}

	return nil
}

// Resource Validations

func (r *Release) ValidateResources(lambdac aws.LambdaAPI, sfnc aws.SFNAPI, s3c aws.S3API) error {
	if err := r.ValidateLambdaFunctionTags(lambdac); err != nil {
		return err
	}

	if err := r.ValidateStepFunctionPath(sfnc); err != nil {
		return err
	}

	if err := r.ValidateLambdaSHA(s3c); err != nil {
		return err
	}

	if err := r.ValidateReleaseSHA(s3c); err != nil {
		return err
	}

	return nil
}

func (r *Release) ValidateLambdaFunctionTags(lambdac aws.LambdaAPI) error {
	project, config, deployer, err := r.LambdaProjectConfigDeployerTags(lambdac)
	if err != nil {
		return err
	}

	if project == nil || config == nil || deployer == nil {
		return fmt.Errorf("ProjectName, ConfigName and or DeployWith tag on lambda is nil")
	}

	if *r.ProjectName != *project {
		return fmt.Errorf("Lambda ProjectName tag incorrect, expecting %v has %v", *r.ProjectName, *project)
	}

	if *r.ConfigName != *config {
		return fmt.Errorf("Lambda ConfigName tag incorrect, expecting %v has %v", *r.ConfigName, *config)
	}

	if "step-deployer" != *deployer {
		return fmt.Errorf("Lambda DeployWith tag incorrect, expecting step-deployer has %v", *deployer)
	}

	return nil
}

func (r *Release) ValidateStepFunctionPath(sfnc aws.SFNAPI) error {
	out, err := sfnc.DescribeStateMachine(&sfn.DescribeStateMachineInput{StateMachineArn: r.StepArn()})

	if err != nil {
		return err
	}

	if out == nil || out.RoleArn == nil {
		return fmt.Errorf("Unknown Step Function Error")
	}

	path := to.ArnPath(*out.RoleArn)

	expected := fmt.Sprintf("/step/%v/%v/", *r.ProjectName, *r.ConfigName)
	if path != expected {
		return fmt.Errorf("Incorrect Step Function Role Path, expecting %v, got %v", expected, path)
	}

	return nil
}

func (r *Release) ValidateLambdaSHA(s3c aws.S3API) error {
	sha, err := s3.GetSHA256(s3c, r.Bucket, r.LambdaZipPath())
	if err != nil {
		return err
	}

	if sha != *r.LambdaSHA256 {
		return fmt.Errorf("Lambda SHA mismatch, expecting %v, got %v", *r.LambdaSHA256, sha)
	}

	return nil
}

func (r *Release) ValidateReleaseSHA(s3c aws.S3API) error {
	var s3_release Release
	err := s3.GetStruct(s3c, r.Bucket, r.ReleasePath(), &s3_release)
	if err != nil {
		return fmt.Errorf("Error Unmarshalling uploaded Release struct with %v", err.Error())
	}

	expected := to.SHA256Struct(s3_release)

	if expected != r.ReleaseSHA256 {
		return fmt.Errorf("Release SHA incorrect expected %v, got %v", expected, r.ReleaseSHA256)
	}

	return nil
}

func (r *Release) LambdaProjectConfigDeployerTags(lambdac aws.LambdaAPI) (*string, *string, *string, error) {
	out, err := lambdac.ListTags(&lambda.ListTagsInput{
		Resource: r.LambdaArn(),
	})

	if err != nil {
		return nil, nil, nil, err
	}

	if out == nil {
		return nil, nil, nil, fmt.Errorf("Unknown Lambda Tags Error")
	}

	return out.Tags["ProjectName"], out.Tags["ConfigName"], out.Tags["DeployWith"], nil
}

//////////
// AWS Methods
//////////

func (release *Release) deployLambdaInput(zip *[]byte) *lambda.UpdateFunctionCodeInput {
	return &lambda.UpdateFunctionCodeInput{
		FunctionName: release.LambdaArn(),
		ZipFile:      *zip,
	}
}

// DeployLambdaCode
func (release *Release) DeployLambdaCode(lambdaClient aws.LambdaAPI, zip *[]byte) error {
	_, err := lambdaClient.UpdateFunctionCode(release.deployLambdaInput(zip))
	return err
}

// DeployLambda uploads new Code to the Lambda
func (release *Release) DeployLambda(lambdaClient aws.LambdaAPI, s3c aws.S3API) error {
	// Download and pass Zip file because lambda might be in another region or account
	zip, err := s3.Get(s3c, release.Bucket, release.LambdaZipPath())
	if err != nil {
		return err
	}

	err = release.DeployLambdaCode(lambdaClient, zip)
	if err != nil {
		return err
	}

	return nil
}

func (release *Release) deployStepFunctionInput() *sfn.UpdateStateMachineInput {
	return &sfn.UpdateStateMachineInput{
		Definition:      to.Strp(to.PrettyJSONStr(release.StateMachineJSON)),
		StateMachineArn: release.StepArn(),
	}
}

// DeployStepFunction updates the step function State Machine
func (release *Release) DeployStepFunction(sfnClient aws.SFNAPI) error {
	_, err := sfnClient.UpdateStateMachine(release.deployStepFunctionInput())

	if err != nil {
		return err
	}

	return nil
}

///////
// Lock
///////

func (release *Release) ReleaseLock(s3c aws.S3API) error {
	return s3.ReleaseLock(s3c, release.Bucket, release.LockPath(), *release.UUID)
}

func (release *Release) GrabLock(s3c aws.S3API) (bool, error) {
	return s3.GrabLock(s3c, release.Bucket, release.LockPath(), *release.UUID)
}

func (release *Release) LockPath() *string {
	s := fmt.Sprintf("%v/lock", *release.rootPath())
	return &s
}

///////
// Lambda
///////

func (release *Release) LambdaZipPath() *string {
	s := fmt.Sprintf("%v/lambda.zip", *release.releasePath())
	return &s
}

func (release *Release) LambdaArn() *string {
	return to.LambdaArn(release.AwsRegion, release.AwsAccountID, release.LambdaName)
}

///////
// Step
///////

func (release *Release) StepArn() *string {
	return to.StepArn(release.AwsRegion, release.AwsAccountID, release.StepFnName)
}

///////
// S3 Release
///////

func (release *Release) rootPath() *string {
	s := fmt.Sprintf("%v/%v/%v", *release.AwsAccountID, *release.ProjectName, *release.ConfigName)
	return &s
}

func (release *Release) releasePath() *string {
	s := fmt.Sprintf("%v/%v", *release.rootPath(), *release.ReleaseId)
	return &s
}

func (release *Release) ReleasePath() *string {
	s := fmt.Sprintf("%v/release", *release.releasePath())
	return &s
}

///////
// Defaults
///////

func (release *Release) SetDefaults(region *string, account *string) {
	if is.EmptyStr(release.UUID) {
		release.UUID = to.TimeUUID("release-")
	}

	if is.EmptyStr(release.AwsRegion) {
		release.AwsRegion = region
	}

	if is.EmptyStr(release.AwsAccountID) {
		release.AwsAccountID = account
	}

	if is.EmptyStr(release.Bucket) && account != nil {
		// default bucket is the default account_id not the release id (which could be in a different account)
		release.Bucket = to.Strp(fmt.Sprintf("coinbase-step-deployer-%v", *account))
	}
}
