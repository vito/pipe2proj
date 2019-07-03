package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/template"

	"github.com/concourse/concourse/atc"
	"github.com/concourse/flag"
	"github.com/jessevdk/go-flags"
	"github.com/sergi/go-diff/diffmatchpatch"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

type Command struct {
	ProjectName string   `long:"project-name" short:"n" required:"true" description:"Name to give to the project, e.g. 'ci'."`
	ProjectPath flag.Dir `long:"project-path" short:"j" required:"true" description:"Project path to convert into."`

	PipelineName   string    `long:"pipeline-name"   short:"p" required:"true" description:"Name to give to the pipeline within the project."`
	PipelineConfig flag.File `long:"pipeline-config" short:"c" required:"true" description:"Path to pipeline config."`

	TaskResources map[string]flag.Dir `long:"task-artifact" short:"t" description:"Mapping from artifact name to local directory, used for converting tasks."`

	TemplatesDir flag.Dir `long:"config-templates" description:"Directory containing templates for pretty-printing configs."`
}

type ProjectConfig struct {
	Name         string
	PipelineName string
}

// PipelineConfig is just atc.Config with omitempty everywhere.
type PipelineConfig struct {
	Groups        atc.GroupConfigs    `yaml:"groups,omitempty"`
	Resources     atc.ResourceConfigs `yaml:"resources,omitempty"`
	ResourceTypes atc.ResourceTypes   `yaml:"resource_types,omitempty"`
	Jobs          atc.JobConfigs      `yaml:"jobs,omitempty"`
}

type AnonymousResourceConfig struct {
	Public       bool        `yaml:"public,omitempty"`
	WebhookToken string      `yaml:"webhook_token,omitempty"`
	Type         string      `yaml:"type" json:"type"`
	Source       atc.Source  `yaml:"source" json:"source"`
	CheckEvery   string      `yaml:"check_every,omitempty"`
	CheckTimeout string      `yaml:"check_timeout,omitempty"`
	Tags         atc.Tags    `yaml:"tags,omitempty"`
	Version      atc.Version `yaml:"version,omitempty"`
	Icon         string      `yaml:"icon,omitempty"`
}

func (cmd Command) Execute([]string) error {
	logrus.SetLevel(logrus.DebugLevel)

	var tmpl *template.Template
	if cmd.TemplatesDir != "" {
		tmpl = template.New("root").Funcs(template.FuncMap{
			"yaml": func(indent int, x interface{}) (string, error) {
				payload, err := yaml.Marshal(x)
				if err != nil {
					return "", err
				}

				trimmed := strings.TrimSuffix(string(payload), "\n")

				var indented string
				for _, line := range strings.Split(trimmed, "\n") {
					if len(indented) > 0 {
						indented += "\n" + strings.Repeat("  ", indent)
					}

					indented += line
				}

				return indented, nil
			},
		})

		var err error
		tmpl, err = tmpl.ParseGlob(cmd.TemplatesDir.Path() + "/*.tmpl")
		if err != nil {
			return fmt.Errorf("parsing templates: %s", err)
		}
	}

	var config PipelineConfig
	payload, err := ioutil.ReadFile(cmd.PipelineConfig.Path())
	if err != nil {
		return fmt.Errorf("read: %s", err)
	}

	err = yaml.Unmarshal(payload, &config)
	if err != nil {
		return fmt.Errorf("unmarshal: %s", err)
	}

	pipelinesPath := filepath.Join(cmd.ProjectPath.Path(), "pipelines")
	tasksPath := filepath.Join(cmd.ProjectPath.Path(), "tasks")
	scriptsPath := filepath.Join(cmd.ProjectPath.Path(), "tasks", "scripts")
	resourcesPath := filepath.Join(cmd.ProjectPath.Path(), "resources")
	resourceTypesPath := filepath.Join(cmd.ProjectPath.Path(), "resource-types")

	if len(config.Resources) > 0 {
		err := os.MkdirAll(resourcesPath, 0755)
		if err != nil {
			return fmt.Errorf("creating resources directory: %s", err)
		}
	}

	if len(config.ResourceTypes) > 0 {
		err := os.MkdirAll(resourceTypesPath, 0755)
		if err != nil {
			return fmt.Errorf("creating resource types directory: %s", err)
		}
	}

	for _, res := range config.Resources {
		resourcePath := filepath.Join(resourcesPath, res.Name+".yml")

		logrus.WithFields(logrus.Fields{
			"name": res.Name,
		}).Info("converting resource")

		err := render(resourcePath, tmpl, "resource.tmpl", anonymize(res))
		if err != nil {
			return fmt.Errorf("failed to write resource: %s", err)
		}
	}

	for _, res := range config.ResourceTypes {
		resourceTypePath := filepath.Join(resourceTypesPath, res.Name+".yml")

		logrus.WithFields(logrus.Fields{
			"name": res.Name,
		}).Info("converting resource type")

		err := render(resourceTypePath, tmpl, "resource.tmpl", anonymize(res))
		if err != nil {
			return fmt.Errorf("failed to write resource: %s", err)
		}
	}

	newJobs := []atc.JobConfig{}
	for _, j := range config.Jobs {
		newPlan, err := walkPlan(atc.PlanConfig{Do: &j.Plan}, func(p atc.PlanConfig) (atc.PlanConfig, error) {
			if p.Task == "" {
				return p, nil
			}

			if p.TaskConfigPath == "" {
				return p, nil
			}

			log := logrus.WithFields(logrus.Fields{
				"file": p.TaskConfigPath,
			})

			taskName := strings.TrimSuffix(filepath.Base(p.TaskConfigPath), ".yml")
			taskPath := filepath.Join(tasksPath, taskName+".yml")

			for artifactName, localDir := range cmd.TaskResources {
				prefix := artifactName + "/"

				if !strings.HasPrefix(p.TaskConfigPath, prefix) {
					continue
				}

				log.Info("converting task")

				localTaskPath := filepath.Join(localDir.Path(), strings.TrimPrefix(p.TaskConfigPath, prefix))

				taskPayload, err := ioutil.ReadFile(localTaskPath)
				if err != nil {
					return p, fmt.Errorf("loading task: %s", err)
				}

				var taskConfig atc.TaskConfig
				err = yaml.Unmarshal(taskPayload, &taskConfig)
				if err != nil {
					return p, fmt.Errorf("parsing task config: %s", err)
				}

				if strings.HasPrefix(taskConfig.Run.Path, prefix) {
					log.WithFields(logrus.Fields{
						"script": taskConfig.Run.Path,
					}).Info("converting script")

					localScriptPath := filepath.Join(localDir.Path(), strings.TrimPrefix(taskConfig.Run.Path, prefix))
					scriptPayload, err := ioutil.ReadFile(localScriptPath)
					if err != nil {
						return p, fmt.Errorf("loading script: %s", err)
					}

					scriptName := filepath.Base(taskConfig.Run.Path)
					scriptPath := filepath.Join(scriptsPath, scriptName)
					err = syncFile(scriptPath, scriptPayload)
					if err != nil {
						return p, fmt.Errorf("failed to sync script: %s", err)
					}

					taskConfig.Inputs = append([]atc.TaskInputConfig{{Name: cmd.ProjectName}}, taskConfig.Inputs...)
					taskConfig.Run.Path = filepath.Join(cmd.ProjectName, "tasks", "scripts", scriptName)
				}

				err = render(taskPath, tmpl, "task.tmpl", taskConfig)
				if err != nil {
					return p, fmt.Errorf("failed to write task: %s", err)
				}

				p.TaskConfigPath = ""
				p.Task = taskName
			}

			return p, nil
		})
		if err != nil {
			return err
		}

		j.Plan = *newPlan.Do
		newJobs = append(newJobs, j)
	}

	config.Resources = nil
	config.ResourceTypes = nil
	config.Jobs = newJobs

	pipelinePath := filepath.Join(pipelinesPath, cmd.PipelineName+".yml")
	err = render(pipelinePath, tmpl, "pipeline.tmpl", config)
	if err != nil {
		return fmt.Errorf("failed to sync pipeline: %s", err)
	}

	return nil
}

func render(dest string, tmpl *template.Template, name string, val interface{}) error {
	payload, err := yaml.Marshal(val)
	if err != nil {
		return err
	}

	prettyPayload := new(bytes.Buffer)
	if tmpl != nil {
		err = tmpl.ExecuteTemplate(prettyPayload, name, val)
		if err != nil {
			return fmt.Errorf("failed to execute template: %s", err)
		}

		// verify that the template is equivalent
		var x, y interface{}
		err = yaml.Unmarshal(prettyPayload.Bytes(), &x)
		if err != nil {
			return fmt.Errorf("template rendered invalid YAML: %s", err)
		}

		err = yaml.Unmarshal(payload, &y)
		if err != nil {
			return fmt.Errorf("template rendered invalid YAML: %s", err)
		}

		if !reflect.DeepEqual(x, y) {
			return fmt.Errorf("pretty-printed value not equvalent to ugly-printed value:\n\n%s\n\npretty value:\n\n%s", payload, prettyPayload.Bytes())
		}
	} else {
		_, err = prettyPayload.Write(payload)
		if err != nil {
			return err
		}
	}

	err = syncFile(dest, prettyPayload.Bytes())
	if err != nil {
		return fmt.Errorf("failed to write: %s", err)
	}

	return nil
}

func syncFile(path string, payload []byte) error {
	parent := filepath.Dir(path)
	if _, err := os.Stat(parent); os.IsNotExist(err) {
		err = os.MkdirAll(parent, 0755)
		if err != nil {
			return err
		}
	}

	existingPayload, err := ioutil.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else {
		dmp := diffmatchpatch.New()

		diffs := dmp.DiffMain(string(existingPayload), string(payload), true)

		if !bytes.Equal(existingPayload, payload) {
			return fmt.Errorf("path %s already has different content:\n\n%s", path, dmp.DiffPrettyText(diffs))
		}
	}

	err = ioutil.WriteFile(path, payload, 0644)
	if err != nil {
		return fmt.Errorf("failed to write file: %s", err)
	}

	return nil
}

func anonymize(resource interface{}) AnonymousResourceConfig {
	payload, err := yaml.Marshal(resource)
	if err != nil {
		panic(err)
	}

	var anon AnonymousResourceConfig
	err = yaml.Unmarshal(payload, &anon)
	if err != nil {
		panic(err)
	}

	return anon
}

func failIf(msg string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, msg, err)
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
}

func ptr(plan atc.PlanConfig) *atc.PlanConfig {
	return &plan
}

func walkPlan(plan atc.PlanConfig, f func(atc.PlanConfig) (atc.PlanConfig, error)) (atc.PlanConfig, error) {
	if plan.Abort != nil {
		walked, err := walkPlan(*plan.Abort, f)
		if err != nil {
			return atc.PlanConfig{}, err
		}

		plan.Abort = ptr(walked)
		return f(plan)
	}

	if plan.Error != nil {
		walked, err := walkPlan(*plan.Error, f)
		if err != nil {
			return atc.PlanConfig{}, err
		}

		plan.Error = ptr(walked)
		return f(plan)
	}

	if plan.Success != nil {
		walked, err := walkPlan(*plan.Success, f)
		if err != nil {
			return atc.PlanConfig{}, err
		}

		plan.Success = ptr(walked)
		return f(plan)
	}

	if plan.Failure != nil {
		walked, err := walkPlan(*plan.Failure, f)
		if err != nil {
			return atc.PlanConfig{}, err
		}

		plan.Failure = ptr(walked)
		return f(plan)
	}

	if plan.Ensure != nil {
		walked, err := walkPlan(*plan.Ensure, f)
		if err != nil {
			return atc.PlanConfig{}, err
		}

		plan.Ensure = ptr(walked)
		return f(plan)
	}

	if plan.Try != nil {
		walked, err := walkPlan(*plan.Try, f)
		if err != nil {
			return atc.PlanConfig{}, err
		}

		plan.Try = ptr(walked)
		return f(plan)
	}

	if plan.Do != nil {
		var plans atc.PlanSequence
		for _, p := range *plan.Do {
			walked, err := walkPlan(p, f)
			if err != nil {
				return atc.PlanConfig{}, err
			}

			plans = append(plans, walked)
		}

		plan.Do = &plans
		return f(plan)
	}

	if plan.Aggregate != nil {
		var plans atc.PlanSequence
		for _, p := range *plan.Aggregate {
			walked, err := walkPlan(p, f)
			if err != nil {
				return atc.PlanConfig{}, err
			}

			plans = append(plans, walked)
		}

		plan.Aggregate = &plans
		return f(plan)
	}

	if plan.InParallel != nil {
		var plans atc.PlanSequence
		for _, p := range plan.InParallel.Steps {
			walked, err := walkPlan(p, f)
			if err != nil {
				return atc.PlanConfig{}, err
			}

			plans = append(plans, walked)
		}

		plan.InParallel.Steps = plans
		return f(plan)
	}

	if plan.Get != "" {
		return f(plan)
	}

	if plan.Put != "" {
		return f(plan)
	}

	if plan.Task != "" {
		return f(plan)
	}

	prettyStep, err := yaml.Marshal(plan)
	if err != nil {
		return atc.PlanConfig{}, err
	}

	return atc.PlanConfig{}, fmt.Errorf("unknown step type:\n\n%s", prettyStep)
}

func main() {
	var cmd Command
	parser := flags.NewParser(&cmd, flags.HelpFlag|flags.PassDoubleDash)
	parser.NamespaceDelimiter = "-"

	args, err := parser.Parse()
	failIf("parse: %s", err)

	err = cmd.Execute(args)
	failIf("error: %s", err)
}
