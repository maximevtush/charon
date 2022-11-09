// Copyright © 2022 Obol Labs Inc.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of  MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for
// more details.
//
// You should have received a copy of the GNU General Public License along with
// this program.  If not, see <http://www.gnu.org/licenses/>.

package compose

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"time"

	"github.com/obolnetwork/charon/app/errors"
	"github.com/obolnetwork/charon/app/log"
	"github.com/obolnetwork/charon/app/z"
)

type AutoConfig struct {
	// Dir is the directory to use for compose artifacts.
	Dir string
	// AlertTimeout is the timeout to collect alerts before shutdown. Zero disables timeout.
	AlertTimeout time.Duration
	// SudoPerms enables changing all compose artefacts file permissions using sudo.
	SudoPerms bool
	// Print generated docker-compose.yml files.
	PrintYML bool
	// RunTmplFunc allows arbitrary overrides in the run step template.
	RunTmplFunc func(*TmplData)
	// DefineTmplFunc allows arbitrary overrides if the define step template.
	DefineTmplFunc func(*TmplData)
}

// Auto runs all three steps (define,lock,run) sequentially with support for detecting alerts.
//
//nolint:gocognit
func Auto(ctx context.Context, conf AutoConfig) error {
	ctx = log.WithTopic(ctx, "auto")

	steps := []struct {
		Name     string
		RunFunc  RunFunc
		TmplFunc func(*TmplData)
		RunStep  bool
	}{
		{
			Name:     "define",
			RunFunc:  Define,
			TmplFunc: conf.DefineTmplFunc,
		}, {
			Name:    "lock",
			RunFunc: Lock,
		}, {
			Name:     "run",
			RunFunc:  Run,
			TmplFunc: conf.RunTmplFunc,
			RunStep:  true,
		},
	}

	for _, step := range steps {
		run := NewRunnerFunc(step.Name, conf.Dir, false, step.RunFunc)
		tmpl, err := run(ctx)
		if err != nil {
			return err
		}

		if conf.SudoPerms {
			if err := fixPerms(ctx, conf.Dir); err != nil {
				return err
			}
		}

		if step.TmplFunc != nil {
			step.TmplFunc(&tmpl)
			err := WriteDockerCompose(conf.Dir, tmpl)
			if err != nil {
				return err
			}
		}

		if conf.PrintYML {
			if err := printDockerCompose(ctx, conf.Dir); err != nil {
				return err
			}
		}

		if step.RunStep { // Continue below if final run step.
			break
		}

		if err := execUp(ctx, conf.Dir); err != nil {
			return err
		}
	}

	if conf.AlertTimeout > 0 {
		// Ensure everything is clean before we start with alert test.
		_ = execDown(ctx, conf.Dir)

		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, conf.AlertTimeout)
		defer cancel()
	}

	alerts := startAlertCollector(ctx, conf.Dir)

	defer func() {
		_ = execDown(context.Background(), conf.Dir)
	}()

	if err := execUp(ctx, conf.Dir); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var (
		alertMsgs    []string
		alertSuccess bool
	)
	for alert := range alerts {
		if alert == alertsPolled {
			alertSuccess = true
		} else {
			alertMsgs = append(alertMsgs, alert)
		}
	}
	if !alertSuccess {
		log.Error(ctx, "Alerts couldn't be polled", nil)
		return nil // TODO(corver): Fix this and error
	} else if len(alertMsgs) > 0 {
		return errors.New("alerts detected", z.Any("alerts", alertMsgs))
	}

	log.Info(ctx, "No alerts detected")

	return nil
}

// printDockerCompose prints the docker-compose.yml file to stdout.
func printDockerCompose(ctx context.Context, dir string) error {
	log.Info(ctx, "Printing docker-compose.yml")
	cmd := exec.CommandContext(ctx, "cat", "docker-compose.yml")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return errors.Wrap(err, "exec cat docker-compose.yml")
	}

	return nil
}

// fixPerms fixes file permissions as a workaround for linux docker by removing
// all restrictions using sudo chmod.
func fixPerms(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "sudo", "chmod", "-R", "a+wrX", ".")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return errors.Wrap(err, "exec sudo chmod")
	}

	return nil
}

// execDown executes `docker-compose down`.
func execDown(ctx context.Context, dir string) error {
	log.Info(ctx, "Executing docker-compose down")

	cmd := exec.CommandContext(ctx, "docker-compose", "down",
		"--remove-orphans",
		"--timeout=2",
	)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "run down")
	}

	return nil
}

// execUp executes `docker-compose up`.
func execUp(ctx context.Context, dir string) error {
	// Build first so containers start at the same time below.
	log.Info(ctx, "Executing docker-compose build")
	cmd := exec.CommandContext(ctx, "docker-compose", "build", "--parallel")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrap(err, "exec docker-compose build", z.Str("output", string(out)))
	}

	log.Info(ctx, "Executing docker-compose up")
	cmd = exec.CommandContext(ctx, "docker-compose", "up",
		"--remove-orphans",
		"--abort-on-container-exit",
		"--quiet-pull",
	)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}

		return errors.Wrap(err, "exec docker-compose up")
	}

	return nil
}

// RunFunc defines a function that generates docker-compose.yml from config and returns the template data.
type RunFunc func(context.Context, string, Config) (TmplData, error)

// NewRunnerFunc returns a function that wraps and runs a run function.
func NewRunnerFunc(topic string, dir string, up bool, runFunc RunFunc,
) func(ctx context.Context) (data TmplData, err error) {
	return func(ctx context.Context) (data TmplData, err error) {
		ctx = log.WithTopic(ctx, topic)

		conf, err := LoadConfig(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return TmplData{}, errors.New("compose config.json not found; maybe try `compose new` first", z.Str("dir", dir))
		} else if err != nil {
			return TmplData{}, err
		}

		log.Info(ctx, "Running compose command", z.Str("command", topic))

		data, err = runFunc(ctx, dir, conf)
		if err != nil {
			return TmplData{}, err
		}

		if up {
			return data, execUp(ctx, dir)
		}

		return data, nil
	}
}
