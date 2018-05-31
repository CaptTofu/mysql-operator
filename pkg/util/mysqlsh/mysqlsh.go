// Copyright 2018 Oracle and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mysqlsh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	utilexec "k8s.io/utils/exec"

	"github.com/oracle/mysql-operator/pkg/cluster/innodb"
)

// Interface is an injectable interface for running mysqlsh commands.
type Interface interface {
	IsClustered(ctx context.Context) bool
	// CreateCluster creates a new InnoDB cluster called
	// innodb.DefaultClusterName.
	CreateCluster(ctx context.Context, opts Options) (*innodb.ClusterStatus, error)
	// GetClusterStatus gets the status of the innodb.DefaultClusterName InnoDB
	// cluster.
	GetClusterStatus(ctx context.Context) (*innodb.ClusterStatus, error)
	// CheckInstanceState verifies the existing data on the instance (specified
	// by URI) does not prevent it from joining a cluster.
	CheckInstanceState(ctx context.Context, uri string) (*innodb.InstanceState, error)
	// AddInstanceToCluster adds the instance (specified by URI) the InnoDB
	// cluster.
	AddInstanceToCluster(ctx context.Context, uri string, opts Options) error
	// RejoinInstanceToCluster rejoins an instance (specified by URI) to the
	// InnoDB cluster.
	RejoinInstanceToCluster(ctx context.Context, uri string, opts Options) error
	// RemoveInstanceFromCluster removes an instance (specified by URI) to the
	// InnoDB cluster.
	RemoveInstanceFromCluster(ctx context.Context, uri string, opts Options) error
	// RebootClusterFromCompleteOutage recovers a cluster when all of its members
	// have failed.
	RebootClusterFromCompleteOutage(ctx context.Context) error
}

// errorRegex is used to parse Python tracebacks generated by mysql-shell.
var errorRegex = regexp.MustCompile(`Traceback.*\n(?:  (.*)\n){1,}(?P<type>[\w\.]+)\: (?P<message>.*)`)

// New creates a new MySQL Shell Interface.
func New(exec utilexec.Interface, uri string) Interface {
	return &runner{exec: exec, uri: uri}
}

// runner implements Interface in terms of exec("mysqlsh").
type runner struct {
	mu   sync.Mutex
	exec utilexec.Interface

	// uri is Uniform Resource Identifier of the MySQL instance to connect to.
	// Format: [user[:pass]]@host[:port][/db].
	uri string
}

func (r *runner) IsClustered(ctx context.Context) bool {
	python := fmt.Sprintf("dba.get_cluster('%s')", innodb.DefaultClusterName)
	_, err := r.run(ctx, python)
	return err == nil
}

func (r *runner) CreateCluster(ctx context.Context, opts Options) (*innodb.ClusterStatus, error) {
	python := fmt.Sprintf("print dba.create_cluster('%s', %s).status()", innodb.DefaultClusterName, opts)
	output, err := r.run(ctx, python)
	if err != nil {
		return nil, err
	}

	// Skip non-json spat out on stdout.
	var jsonData string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "{") {
			jsonData = line
			break
		}
	}
	if jsonData == "" {
		return nil, errors.Errorf("no json found in output: %q", output)
	}

	status := &innodb.ClusterStatus{}
	err = json.Unmarshal([]byte(jsonData), status)
	if err != nil {
		return nil, errors.Wrapf(err, "decoding cluster status output: %q", output)
	}
	return status, nil
}

func (r *runner) GetClusterStatus(ctx context.Context) (*innodb.ClusterStatus, error) {
	python := fmt.Sprintf("print dba.get_cluster('%s').status()", innodb.DefaultClusterName)
	output, err := r.run(ctx, python)
	if err != nil {
		return nil, err
	}

	status := &innodb.ClusterStatus{}
	err = json.Unmarshal(output, status)
	if err != nil {
		return nil, errors.Wrapf(err, "decoding cluster status output: %q", output)
	}

	return status, nil
}

func (r *runner) CheckInstanceState(ctx context.Context, uri string) (*innodb.InstanceState, error) {
	python := fmt.Sprintf("print dba.get_cluster('%s').check_instance_state('%s')", innodb.DefaultClusterName, uri)
	output, err := r.run(ctx, python)
	if err != nil {
		return nil, err
	}

	state := &innodb.InstanceState{}
	err = json.Unmarshal(output, state)
	if err != nil {
		return nil, fmt.Errorf("decoding instance state: %v", err)
	}

	return state, nil
}

func (r *runner) AddInstanceToCluster(ctx context.Context, uri string, opts Options) error {
	python := fmt.Sprintf("dba.get_cluster('%s').add_instance('%s', %s)", innodb.DefaultClusterName, uri, opts)
	_, err := r.run(ctx, python)
	return err
}

func (r *runner) RejoinInstanceToCluster(ctx context.Context, uri string, opts Options) error {
	python := fmt.Sprintf("dba.get_cluster('%s').rejoin_instance('%s', %s)", innodb.DefaultClusterName, uri, opts)
	_, err := r.run(ctx, python)
	return err
}

func (r *runner) RemoveInstanceFromCluster(ctx context.Context, uri string, opts Options) error {
	python := fmt.Sprintf("dba.get_cluster('%s').remove_instance('%s', %s)", innodb.DefaultClusterName, uri, opts)
	_, err := r.run(ctx, python)
	return err
}

// stripPasswordWarning strips the password warning output by mysqlsh due to the
// fact we pass the password as part of the connection URI.
func (r *runner) stripPasswordWarning(in []byte) []byte {
	warning := []byte("mysqlx: [Warning] Using a password on the command line interface can be insecure.\n")
	return bytes.Replace(in, warning, []byte(""), 1)
}

func (r *runner) run(ctx context.Context, python string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	args := []string{"--no-wizard", "--uri", r.uri, "--py", "-e", python}

	cmd := r.exec.CommandContext(ctx, "mysqlsh", args...)

	cmd.SetStdout(stdout)
	cmd.SetStderr(stderr)

	glog.V(6).Infof("Running command: mysqlsh %v", args)
	err := cmd.Run()
	glog.V(6).Infof("    stdout: %s\n    stderr: %s\n    err: %s", stdout, stderr, err)
	if err != nil {
		underlying := NewErrorFromStderr(stderr.String())
		if underlying != nil {
			return nil, errors.WithStack(underlying)
		}
	}

	return r.stripPasswordWarning(stdout.Bytes()), err
}

func (r *runner) RebootClusterFromCompleteOutage(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	// NOTE(apryde): This is implemented in SQL rather than as a call to
	// dba.reboot_cluster_from_complete_outage() due to https://bugs.mysql.com/90793.
	sql := strings.Join([]string{
		"RESET PERSIST group_replication_bootstrap_group;",
		"SET GLOBAL group_replication_bootstrap_group=ON;",
		"start group_replication;",
	}, " ")

	args := []string{"--no-wizard", "--uri", r.uri, "--sql", "-e", sql}

	cmd := r.exec.CommandContext(ctx, "mysqlsh", args...)

	cmd.SetStdout(stdout)
	cmd.SetStderr(stderr)

	glog.V(6).Infof("Running command: mysqlsh %v", args)
	err := cmd.Run()
	glog.V(6).Infof("    stdout: %s\n    stderr: %s\n    err: %s", stdout, stderr, err)
	if err != nil {
		underlying := NewErrorFromStderr(stderr.String())
		if underlying != nil {
			return errors.WithStack(underlying)
		}
	}
	return err
}

// Error holds errors from mysql-shell commands.
type Error struct {
	error
	Type    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

// NewErrorFromStderr parses the given output from mysql-shell into an Error if
// one is present.
func NewErrorFromStderr(stderr string) error {
	matches := errorRegex.FindAllStringSubmatch(stderr, -1)
	if len(matches) == 0 {
		return nil
	}
	result := make(map[string]string)
	for i, name := range errorRegex.SubexpNames() {
		if i != 0 && name != "" {
			result[name] = matches[len(matches)-1][i]
		}
	}
	return &Error{
		Type:    result["type"],
		Message: result["message"],
	}
}
