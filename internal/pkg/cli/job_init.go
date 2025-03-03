// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"

	"github.com/aws/copilot-cli/internal/pkg/docker/dockerfile"

	"github.com/aws/copilot-cli/internal/pkg/docker/dockerengine"

	"github.com/aws/copilot-cli/internal/pkg/aws/sessions"
	"github.com/aws/copilot-cli/internal/pkg/cli/group"
	"github.com/aws/copilot-cli/internal/pkg/config"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation"
	"github.com/aws/copilot-cli/internal/pkg/exec"
	"github.com/aws/copilot-cli/internal/pkg/initialize"
	"github.com/aws/copilot-cli/internal/pkg/manifest"
	"github.com/aws/copilot-cli/internal/pkg/term/color"
	"github.com/aws/copilot-cli/internal/pkg/term/log"
	termprogress "github.com/aws/copilot-cli/internal/pkg/term/progress"
	"github.com/aws/copilot-cli/internal/pkg/term/prompt"
	"github.com/aws/copilot-cli/internal/pkg/term/selector"
	"github.com/aws/copilot-cli/internal/pkg/workspace"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
)

var (
	jobInitSchedulePrompt = "How would you like to " + color.Emphasize("schedule") + " this job?"
	jobInitScheduleHelp   = `How to determine this job's schedule. "Rate" lets you define the time between 
executions and is good for jobs which need to run frequently. "Fixed Schedule"
lets you use a predefined or custom cron schedule and is good for less-frequent 
jobs or those which require specific execution schedules.`

	jobInitTypeHelp = fmt.Sprintf(`A %s is a task which is invoked on a set schedule, with optional retry logic.
To learn more see: https://git.io/JEEU4`, manifest.ScheduledJobType)
)

var jobTypeHints = map[string]string{
	manifest.ScheduledJobType: "Scheduled event to State Machine to Fargate",
}

type initJobVars struct {
	initWkldVars

	timeout  string
	retries  int
	schedule string
}

type initJobOpts struct {
	initJobVars

	// Interfaces to interact with dependencies.
	fs           afero.Fs
	store        store
	init         jobInitializer
	prompt       prompter
	sel          initJobSelector
	dockerEngine dockerEngine
	mftReader    manifestReader

	// Outputs stored on successful actions.
	manifestPath string
	platform     *manifest.PlatformString

	// Init a Dockerfile parser using fs and input path
	initParser func(string) dockerfileParser
}

func newInitJobOpts(vars initJobVars) (*initJobOpts, error) {
	store, err := config.NewStore()
	if err != nil {
		return nil, fmt.Errorf("couldn't connect to config store: %w", err)
	}

	ws, err := workspace.New()
	if err != nil {
		return nil, fmt.Errorf("workspace cannot be created: %w", err)
	}

	p := sessions.NewProvider()
	sess, err := p.Default()
	fs := &afero.Afero{Fs: afero.NewOsFs()}
	if err != nil {
		return nil, err
	}

	jobInitter := &initialize.WorkloadInitializer{
		Store:    store,
		Ws:       ws,
		Prog:     termprogress.NewSpinner(log.DiagnosticWriter),
		Deployer: cloudformation.New(sess),
	}

	prompter := prompt.New()
	sel := selector.NewWorkspaceSelect(prompter, store, ws)

	return &initJobOpts{
		initJobVars: vars,

		fs:           fs,
		store:        store,
		init:         jobInitter,
		prompt:       prompter,
		sel:          sel,
		dockerEngine: dockerengine.New(exec.NewCmd()),
		mftReader:    ws,
		initParser: func(path string) dockerfileParser {
			return dockerfile.New(fs, path)
		},
	}, nil
}

// Validate returns an error if the flag values passed by the user are invalid.
func (o *initJobOpts) Validate() error {
	if o.appName == "" {
		return errNoAppInWorkspace
	}
	if o.wkldType != "" {
		if err := validateJobType(o.wkldType); err != nil {
			return err
		}
	}
	if o.name != "" {
		if err := validateJobName(o.name); err != nil {
			return err
		}
		if err := o.validateDuplicateJob(); err != nil {
			return err
		}
	}
	if o.dockerfilePath != "" && o.image != "" {
		return fmt.Errorf("--%s and --%s cannot be specified together", dockerFileFlag, imageFlag)
	}
	if o.dockerfilePath != "" {
		if _, err := o.fs.Stat(o.dockerfilePath); err != nil {
			return err
		}
	}
	if o.schedule != "" {
		if err := validateSchedule(o.schedule); err != nil {
			return err
		}
	}
	if o.timeout != "" {
		if err := validateTimeout(o.timeout); err != nil {
			return err
		}
	}
	if o.retries < 0 {
		return errors.New("number of retries must be non-negative")
	}
	return nil
}

// Ask prompts for fields that are required but not passed in.
func (o *initJobOpts) Ask() error {
	if err := o.askJobName(); err != nil {
		return err
	}
	localMft, err := o.mftReader.ReadWorkloadManifest(o.name)
	if err == nil {
		jobType, err := localMft.WorkloadType()
		if err != nil {
			return fmt.Errorf(`read "type" field for job %s from local manifest: %w`, o.name, err)
		}
		o.wkldType = jobType
		log.Infof("Manifest file for job %s already exists. Skipping configuration.\n", o.name)
		return nil
	}
	var (
		errNotFound          *workspace.ErrFileNotExists
		errWorkspaceNotFound *workspace.ErrWorkspaceNotFound
	)
	if !errors.As(err, &errNotFound) && !errors.As(err, &errWorkspaceNotFound) {
		return fmt.Errorf("read manifest file for job %s: %w", o.name, err)
	}
	if err := o.askJobType(); err != nil {
		return err
	}
	dfSelected, err := o.askDockerfile()
	if err != nil {
		return err
	}
	if !dfSelected {
		if err := o.askImage(); err != nil {
			return err
		}
	}
	if err := o.askSchedule(); err != nil {
		return err
	}
	return nil
}

// Execute writes the job's manifest file, creates an ECR repo, and stores the name in SSM.
func (o *initJobOpts) Execute() error {
	// Check for a valid healthcheck and add it to the opts.
	var hc manifest.ContainerHealthCheck
	var err error
	if o.dockerfilePath != "" {
		hc, err = parseHealthCheck(o.initParser(o.dockerfilePath))
		if err != nil {
			log.Warningf("Cannot parse the HEALTHCHECK instruction from the Dockerfile: %v\n", err)
		}
	}
	// If the user passes in an image, their docker engine isn't necessarily running, and we can't do anything with the platform because we're not building the Docker image.
	if o.image == "" {
		platform, err := legitimizePlatform(o.dockerEngine, o.wkldType)
		if err != nil {
			return err
		}
		if platform != "" {
			o.platform = &platform
		}
	}
	manifestPath, err := o.init.Job(&initialize.JobProps{
		WorkloadProps: initialize.WorkloadProps{
			App:            o.appName,
			Name:           o.name,
			Type:           o.wkldType,
			DockerfilePath: o.dockerfilePath,
			Image:          o.image,
			Platform: manifest.PlatformArgsOrString{
				PlatformString: o.platform,
			},
		},

		Schedule:    o.schedule,
		HealthCheck: hc,
		Timeout:     o.timeout,
		Retries:     o.retries,
	})
	if err != nil {
		return err
	}
	o.manifestPath = manifestPath
	return nil
}

// RecommendActions returns follow-up actions the user can take after successfully executing the command.
func (o *initJobOpts) RecommendActions() error {
	logRecommendedActions([]string{
		fmt.Sprintf("Update your manifest %s to change the defaults.", color.HighlightResource(o.manifestPath)),
		fmt.Sprintf("Run %s to deploy your job to a %s environment.",
			color.HighlightCode(fmt.Sprintf("copilot job deploy --name %s --env %s", o.name, defaultEnvironmentName)),
			defaultEnvironmentName),
	})
	return nil
}

func (o *initJobOpts) validateDuplicateJob() error {
	_, err := o.store.GetJob(o.appName, o.name)
	if err == nil {
		log.Errorf(`It seems like you are trying to init a job that already exists.
To recreate the job, please run:
1. %s. Note: The manifest file will not be deleted and will be used in Step 2.
If you'd prefer a new default manifest, please manually delete the existing one.
2. And then %s
`,
			color.HighlightCode(fmt.Sprintf("copilot job delete --name %s", o.name)),
			color.HighlightCode(fmt.Sprintf("copilot job init --name %s", o.name)))
		return fmt.Errorf("job %s already exists", color.HighlightUserInput(o.name))
	}

	var errNoSuchJob *config.ErrNoSuchJob
	if !errors.As(err, &errNoSuchJob) {
		return fmt.Errorf("validate if job exists: %w", err)
	}
	return nil
}

func (o *initJobOpts) askJobType() error {
	if o.wkldType != "" {
		return nil
	}
	// short circuit since there's only one valid job type.
	o.wkldType = manifest.ScheduledJobType
	return nil
}

func (o *initJobOpts) askJobName() error {
	if o.name != "" {
		return nil
	}
	name, err := o.prompt.Get(
		fmt.Sprintf(fmtWkldInitNamePrompt, color.Emphasize("name"), "job"),
		fmt.Sprintf(fmtWkldInitNameHelpPrompt, "job", o.appName),
		func(val interface{}) error {
			return validateJobName(val)
		},
		prompt.WithFinalMessage("Job name:"),
	)
	if err != nil {
		return fmt.Errorf("get job name: %w", err)
	}
	o.name = name
	return o.validateDuplicateJob()
}

func (o *initJobOpts) askImage() error {
	if o.image != "" {
		return nil
	}
	image, err := o.prompt.Get(wkldInitImagePrompt, wkldInitImagePromptHelp, nil,
		prompt.WithFinalMessage("Image:"))
	if err != nil {
		return fmt.Errorf("get image location: %w", err)
	}
	o.image = image
	return nil
}

// isDfSelected indicates if any Dockerfile is in use.
func (o *initJobOpts) askDockerfile() (isDfSelected bool, err error) {
	if o.dockerfilePath != "" || o.image != "" {
		return true, nil
	}
	if err = o.dockerEngine.CheckDockerEngineRunning(); err != nil {
		var errDaemon *dockerengine.ErrDockerDaemonNotResponsive
		switch {
		case errors.Is(err, dockerengine.ErrDockerCommandNotFound):
			log.Info("Docker command is not found; Copilot won't build from a Dockerfile.\n")
			return false, nil
		case errors.As(err, &errDaemon):
			log.Info("Docker daemon is not responsive; Copilot won't build from a Dockerfile.\n")
			return false, nil
		default:
			return false, fmt.Errorf("check if docker engine is running: %w", err)
		}
	}
	df, err := o.sel.Dockerfile(
		fmt.Sprintf(fmtWkldInitDockerfilePrompt, color.HighlightUserInput(o.name)),
		fmt.Sprintf(fmtWkldInitDockerfilePathPrompt, color.HighlightUserInput(o.name)),
		wkldInitDockerfileHelpPrompt,
		wkldInitDockerfilePathHelpPrompt,
		func(v interface{}) error {
			return validatePath(afero.NewOsFs(), v)
		},
	)
	if err != nil {
		return false, fmt.Errorf("select Dockerfile: %w", err)
	}
	if df == selector.DockerfilePromptUseImage {
		return false, nil
	}
	o.dockerfilePath = df
	return true, nil
}

func (o *initJobOpts) askSchedule() error {
	if o.schedule != "" {
		return nil
	}
	schedule, err := o.sel.Schedule(
		jobInitSchedulePrompt,
		jobInitScheduleHelp,
		validateSchedule,
		validateRate,
	)
	if err != nil {
		return fmt.Errorf("get schedule: %w", err)
	}

	o.schedule = schedule
	return nil
}

func jobTypePromptOpts() []prompt.Option {
	var options []prompt.Option
	for _, jobType := range manifest.JobTypes {
		options = append(options, prompt.Option{
			Value: jobType,
			Hint:  jobTypeHints[jobType],
		})
	}
	return options
}

// buildJobInitCmd builds the command for creating a new job.
func buildJobInitCmd() *cobra.Command {
	vars := initJobVars{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Creates a new scheduled job in an application.",
		Example: `
  Create a "reaper" scheduled task to run once per day.
  /code $ copilot job init --name reaper --dockerfile ./frontend/Dockerfile --schedule "every 2 hours"

  Create a "report-generator" scheduled task with retries.
  /code $ copilot job init --name report-generator --schedule "@monthly" --retries 3 --timeout 900s`,
		RunE: runCmdE(func(cmd *cobra.Command, args []string) error {
			opts, err := newInitJobOpts(vars)
			if err != nil {
				return err
			}
			return run(opts)
		}),
	}
	cmd.Flags().StringVarP(&vars.appName, appFlag, appFlagShort, tryReadingAppName(), appFlagDescription)
	cmd.Flags().StringVarP(&vars.name, nameFlag, nameFlagShort, "", jobFlagDescription)
	cmd.Flags().StringVarP(&vars.wkldType, jobTypeFlag, typeFlagShort, "", jobTypeFlagDescription)
	cmd.Flags().StringVarP(&vars.dockerfilePath, dockerFileFlag, dockerFileFlagShort, "", dockerFileFlagDescription)
	cmd.Flags().StringVarP(&vars.schedule, scheduleFlag, scheduleFlagShort, "", scheduleFlagDescription)
	cmd.Flags().StringVar(&vars.timeout, timeoutFlag, "", timeoutFlagDescription)
	cmd.Flags().IntVar(&vars.retries, retriesFlag, 0, retriesFlagDescription)
	cmd.Flags().StringVarP(&vars.image, imageFlag, imageFlagShort, "", imageFlagDescription)

	cmd.Annotations = map[string]string{
		"group": group.Develop,
	}

	return cmd
}
