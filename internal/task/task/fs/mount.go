/*
 *  Copyright (c) 2021 NetEase Inc.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

/*
 * Project: CurveAdm
 * Created Date: 2021-10-15
 * Author: Jingli Chen (Wine93)
 */

package fs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opencurve/curveadm/cli/cli"
	"github.com/opencurve/curveadm/internal/common"
	comm "github.com/opencurve/curveadm/internal/common"
	"github.com/opencurve/curveadm/internal/configure"
	"github.com/opencurve/curveadm/internal/configure/topology"
	"github.com/opencurve/curveadm/internal/errno"
	"github.com/opencurve/curveadm/internal/task/context"
	"github.com/opencurve/curveadm/internal/task/scripts"
	"github.com/opencurve/curveadm/internal/task/step"
	"github.com/opencurve/curveadm/internal/task/task"
	"github.com/opencurve/curveadm/internal/task/task/checker"
	"github.com/opencurve/curveadm/internal/utils"
	"github.com/opencurve/curveadm/pkg/module"
)

const (
	FORMAT_MOUNT_OPTION = "type=bind,source=%s,target=%s,bind-propagation=rshared"

	CLIENT_CONFIG_DELIMITER  = "="
	TOOLSV2_CONFIG_DELIMITER = ":"

	KEY_CURVEBS_CLUSTER = "curvebs.cluster"

	CURVEBS_CONF_PATH = "/etc/curve/client.conf"

	CURVEFS_LIST_FS = "curvefs_tool list-fs"

	CHECK_MOUTPOINT_TIMES = 3
)

type (
	MountOptions struct {
		Host        string
		MountFSName string
		MountFSType string
		MountPoint  string
	}

	step2InsertClient struct {
		curveadm    *cli.CurveAdm
		options     MountOptions
		config      *configure.ClientConfig
		containerId *string
	}

	AuxInfo struct {
		FSName     string `json:"fsname"`
		MountPoint string `json:"mount_point,"`
		Config     string `json:"config,omitempty"` // TODO(P1)
	}

	CheckMountDone struct {
		ContainerId *string
		MountPoint  string
		module.ExecOptions
	}
)

var (
	// TODO(P1): use template
	FORMAT_FUSE_ARGS = []string{
		"-f",
		"-o default_permissions",
		"-o allow_other",
		"-o fsname=%s", // fsname
		"-o fstype=%s", // fstype, `s3` or `volume`
		"-o user=curvefs",
		"-o conf=%s", // config path
		"%s",         // mount path
	}
)

func getMountCommand(cc *configure.ClientConfig, mountFSName string, mountFSType string, mountPoint string) string {
	format := strings.Join(FORMAT_FUSE_ARGS, " ")
	fuseArgs := fmt.Sprintf(format, mountFSName, mountFSType,
		configure.GetFSClientConfPath(), configure.GetFSClientMountPath(mountPoint))
	return fmt.Sprintf("/client.sh %s %s --role=client --args='%s'", mountFSName, mountFSType, fuseArgs)
}

func getMountVolumes(cc *configure.ClientConfig) []step.Volume {
	volumes := []step.Volume{}
	prefix := configure.GetFSClientPrefix()
	logDir := cc.GetLogDir()
	dataDir := cc.GetDataDir()
	coreDir := cc.GetCoreDir()

	if len(logDir) > 0 {
		volumes = append(volumes, step.Volume{
			HostPath:      logDir,
			ContainerPath: fmt.Sprintf("%s/logs", prefix),
		})
	}

	if len(dataDir) > 0 {
		volumes = append(volumes, step.Volume{
			HostPath:      dataDir,
			ContainerPath: fmt.Sprintf("%s/data", prefix),
		})
	}

	if len(coreDir) > 0 {
		volumes = append(volumes, step.Volume{
			HostPath:      coreDir,
			ContainerPath: cc.GetCoreLocateDir(),
		})
	}

	return volumes
}

func newMutate(cc *configure.ClientConfig, delimiter string) step.Mutate {
	serviceConfig := cc.GetServiceConfig()
	return func(in, key, value string) (out string, err error) {
		if len(key) == 0 {
			out = in
			return
		}

		// replace config
		v, ok := serviceConfig[strings.ToLower(key)]
		if ok {
			value = v
		}

		out = fmt.Sprintf("%s%s%s", key, delimiter, value)
		return
	}
}

func newCurveBSMutate(cc *configure.ClientConfig, delimiter string) step.Mutate {
	serviceConfig := cc.GetServiceConfig()

	// we need `curvebs.cluster` if fstype is volume
	if serviceConfig[KEY_CURVEBS_CLUSTER] == "" {
		return func(in, key, value string) (out string, err error) {
			err = errors.New("need `curvebs.cluster` if fstype is `volume`")
			return
		}
	}

	bsClientItems := map[string]string{
		"mds.listen.addr": KEY_CURVEBS_CLUSTER,
	}
	bsClientFixedOptions := map[string]string{
		"mds.registerToMDS":     "false",
		"global.logging.enable": "false",
	}
	return func(in, key, value string) (out string, err error) {
		if len(key) == 0 {
			out = in
			return
		}

		_, ok := bsClientFixedOptions[key]
		if ok {
			value = bsClientFixedOptions[key]
		} else {
			replaceKey := key
			if bsClientItems[key] != "" {
				replaceKey = bsClientItems[key]
			}

			v, ok := serviceConfig[replaceKey]
			if ok {
				value = v
			}
		}

		out = fmt.Sprintf("%s%s%s", key, delimiter, value)
		return
	}
}

func newToolsMutate(cc *configure.ClientConfig, delimiter string) step.Mutate {
	clientConfig := cc.GetServiceConfig()
	tools2client := map[string]string{
		"mdsAddr":       "mdsOpt.rpcRetryOpt.addrs",
		"volumeCluster": KEY_CURVEBS_CLUSTER,
	}
	return func(in, key, value string) (out string, err error) {
		if len(key) == 0 {
			out = in
			return
		}
		replaceKey := key
		if tools2client[key] != "" {
			replaceKey = tools2client[key]
		}
		v, ok := clientConfig[strings.ToLower(replaceKey)]
		if ok {
			value = v
		}
		out = fmt.Sprintf("%s%s%s", key, delimiter, value)
		return
	}
}

func newToolsV2Mutate(cc *configure.ClientConfig, delimiter string) step.Mutate {
	clientConfig := cc.GetServiceConfig()
	tools2client := map[string]string{
		"mdsAddr": "mdsOpt.rpcRetryOpt.addrs",
	}
	return func(in, key, value string) (out string, err error) {
		if len(key) == 0 {
			out = in
			return
		}
		replaceKey := strings.TrimSpace(key)
		if tools2client[strings.TrimSpace(key)] != "" {
			replaceKey = tools2client[strings.TrimSpace(key)]
		}
		v, ok := clientConfig[strings.ToLower(replaceKey)]
		if ok {
			value = v
		}
		out = fmt.Sprintf("%s%s %s", key, delimiter, value)
		return
	}
}

func mountPoint2ContainerName(mountPoint string) string {
	return fmt.Sprintf("curvefs-filesystem-%s", utils.MD5Sum(mountPoint))
}

func checkMountStatus(mountPoint string, out *string) step.LambdaType {
	return func(ctx *context.Context) error {
		if len(*out) == 0 {
			return nil
		}
		return errno.ERR_FS_PATH_ALREADY_MOUNTED.F("mountPath: %s", mountPoint)
	}
}

func getEnvironments(cc *configure.ClientConfig) []string {
	envs := []string{
		"LD_PRELOAD=/usr/local/lib/libjemalloc.so",
	}
	env := cc.GetEnvironments()
	if len(env) > 0 {
		envs = append(envs, strings.Split(env, " ")...)
	}
	return envs
}

func (s *step2InsertClient) Execute(ctx *context.Context) error {
	config := s.config
	curveadm := s.curveadm
	options := s.options
	fsId := curveadm.GetFilesystemId(options.Host, options.MountPoint)

	auxInfo := &AuxInfo{
		FSName:     options.MountFSName,
		MountPoint: options.MountPoint,
	}
	bytes, err := json.Marshal(auxInfo)
	if err != nil {
		return errno.ERR_ENCODE_VOLUME_INFO_TO_JSON_FAILED.E(err)
	}

	err = curveadm.Storage().InsertClient(fsId, config.GetKind(),
		options.Host, *s.containerId, string(bytes))
	if err != nil {
		return errno.ERR_INSERT_CLIENT_FAILED.E(err)
	}
	return nil
}

func checkStartContainerStatus(success *bool, out *string) step.LambdaType {
	return func(ctx *context.Context) error {
		if *success {
			return nil
		} else if strings.Contains(*out, "CREATEFS FAILED") {
			return errno.ERR_CREATE_FILESYSTEM_FAILED
		}
		return errno.ERR_MOUNT_FILESYSTEM_FAILED.S(*out)
	}
}

func NewMountFSTask(curveadm *cli.CurveAdm, cc *configure.ClientConfig) (*task.Task, error) {
	options := curveadm.MemStorage().Get(comm.KEY_MOUNT_OPTIONS).(MountOptions)
	hc, err := curveadm.GetHost(options.Host)
	if err != nil {
		return nil, err
	}

	// new task
	mountPoint := options.MountPoint
	mountFSName := options.MountFSName
	mountFSType := options.MountFSType
	subname := fmt.Sprintf("mountFSName=%s mountFSType=%s mountPoint=%s", mountFSName, mountFSType, mountPoint)
	t := task.NewTask("Mount FileSystem", subname, hc.GetSSHConfig())

	// add step to task
	var containerId, out string
	var success bool
	root := configure.GetFSProjectRoot()
	prefix := configure.GetFSClientPrefix()
	containerMountPath := configure.GetFSClientMountPath(mountPoint)
	containerName := mountPoint2ContainerName(mountPoint)
	createfsScript := scripts.SCRIPT_CREATEFS
	createfsScriptPath := "/client.sh"

	t.AddStep(&step.EngineInfo{
		Success:     &success,
		Out:         &out,
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.Lambda{
		Lambda: checker.CheckEngineInfo(options.Host, curveadm.ExecOptions().ExecWithEngine, &success, &out),
	})
	t.AddStep(&step.ListContainers{
		ShowAll:     true,
		Format:      "'{{.Status}}'",
		Filter:      fmt.Sprintf("name=%s", containerName),
		Out:         &out,
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.Lambda{
		Lambda: checkMountStatus(mountPoint, &out),
	})
	t.AddStep(&step.PullImage{
		Image:       cc.GetContainerImage(),
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.CreateContainer{
		Image:             cc.GetContainerImage(),
		Command:           getMountCommand(cc, mountFSName, mountFSType, mountPoint),
		Entrypoint:        "/bin/bash",
		Envs:              getEnvironments(cc),
		Init:              true,
		Name:              mountPoint2ContainerName(mountPoint),
		Mount:             fmt.Sprintf(FORMAT_MOUNT_OPTION, mountPoint, containerMountPath),
		Volumes:           getMountVolumes(cc),
		Devices:           []string{"/dev/fuse"},
		SecurityOptions:   []string{"apparmor:unconfined"},
		LinuxCapabilities: []string{"SYS_ADMIN"},
		Ulimits:           []string{"core=-1"},
		Pid:               cc.GetContainerPid(),
		Privileged:        true,
		Out:               &containerId,
		ExecOptions:       curveadm.ExecOptions(),
		Restart:           common.POLICY_ALWAYS_RESTART,
	})
	t.AddStep(&step2InsertClient{
		curveadm:    curveadm,
		options:     options,
		config:      cc,
		containerId: &containerId,
	})

	if mountFSType == "volume" {
		t.AddStep(&step.SyncFile{ // sync volume client config
			ContainerSrcId:    &containerId,
			ContainerSrcPath:  fmt.Sprintf("%s/conf/curvebs-client.conf", root),
			ContainerDestId:   &containerId,
			ContainerDestPath: CURVEBS_CONF_PATH,
			KVFieldSplit:      CLIENT_CONFIG_DELIMITER,
			Mutate:            newCurveBSMutate(cc, CLIENT_CONFIG_DELIMITER),
			ExecOptions:       curveadm.ExecOptions(),
		})
	}

	t.AddStep(&step.SyncFile{ // sync service config
		ContainerSrcId:    &containerId,
		ContainerSrcPath:  fmt.Sprintf("%s/conf/client.conf", root),
		ContainerDestId:   &containerId,
		ContainerDestPath: fmt.Sprintf("%s/conf/client.conf", prefix),
		KVFieldSplit:      CLIENT_CONFIG_DELIMITER,
		Mutate:            newMutate(cc, CLIENT_CONFIG_DELIMITER),
		ExecOptions:       curveadm.ExecOptions(),
	})
	t.AddStep(&step.SyncFile{ // sync tools config
		ContainerSrcId:    &containerId,
		ContainerSrcPath:  fmt.Sprintf("%s/conf/tools.conf", root),
		ContainerDestId:   &containerId,
		ContainerDestPath: topology.GetCurveFSProjectLayout().ToolsConfSystemPath,
		KVFieldSplit:      CLIENT_CONFIG_DELIMITER,
		Mutate:            newToolsMutate(cc, CLIENT_CONFIG_DELIMITER),
		ExecOptions:       curveadm.ExecOptions(),
	})
	t.AddStep(&step.TrySyncFile{ // sync toolsv2 config
		ContainerSrcId:    &containerId,
		ContainerSrcPath:  fmt.Sprintf("%s/conf/curve.yaml", root),
		ContainerDestId:   &containerId,
		ContainerDestPath: topology.GetCurveFSProjectLayout().ToolsV2ConfSystemPath,
		KVFieldSplit:      TOOLSV2_CONFIG_DELIMITER,
		Mutate:            newToolsV2Mutate(cc, TOOLSV2_CONFIG_DELIMITER),
		ExecOptions:       curveadm.ExecOptions(),
	})
	t.AddStep(&step.InstallFile{ // install client.sh shell
		ContainerId:       &containerId,
		ContainerDestPath: createfsScriptPath,
		Content:           &createfsScript,
		ExecOptions:       curveadm.ExecOptions(),
	})
	t.AddStep(&step.StartContainer{
		ContainerId: &containerId,
		Success:     &success,
		Out:         &out,
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.Lambda{
		Lambda: checkStartContainerStatus(&success, &out),
	})
	t.AddStep(&CheckMountDone{
		ContainerId: &containerId,
		MountPoint:  mountPoint,
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.UpdateContainer{
		ContainerId: &containerId,
		Restart:     comm.POLICY_UNLESS_STOPPED,
		ExecOptions: curveadm.ExecOptions(),
	})

	return t, nil

}

func (s *CheckMountDone) Execute(ctx *context.Context) error {
	steps := []task.Step{}
	var success bool
	var out, hostname string
	// get hostname
	steps = append(steps, &step.Hostname{
		Success:     &success,
		Out:         &hostname,
		ExecOptions: s.ExecOptions,
	})
	for i := 0; i < CHECK_MOUTPOINT_TIMES; i++ {
		steps = append(steps, &step.ContainerExec{
			ContainerId: s.ContainerId,
			Command:     CURVEFS_LIST_FS,
			Success:     &success,
			Out:         &out,
			ExecOptions: s.ExecOptions,
		})
	}
	// list fs 3 times to check mountpoint
	// if no this mountpoint stop container
	steps = append(steps, &step.StopContainer{
		ContainerId: *s.ContainerId,
		ExecOptions: s.ExecOptions,
	})
	mountPoint := configure.GetFSClientMountPath(s.MountPoint)
	for _, step := range steps {
		time.Sleep(time.Duration(1) * time.Second)
		err := step.Execute(ctx)
		if err == nil && success && out != "" {
			if checkMountpointExist(out, mountPoint, hostname) {
				return nil
			}
		}
	}
	return errno.ERR_MOUNT_FILESYSTEM_FAILED
}

const (
	JSON_FS_INFO     = "fsInfo"
	JSON_MOUNT_NUM   = "mountNum"
	JSON_MOUNTPOINTS = "mountpoints"
	JSON_HOSTNAME    = "hostname"
	JSON_PATH        = "path"
)

func checkMountpointExist(out, path, hostname string) bool {
	var jsonMap map[string]interface{}
	err := json.Unmarshal([]byte(out), &jsonMap)
	if err != nil || jsonMap[JSON_FS_INFO] == nil {
		return false
	}

	if !utils.IsAnySlice(jsonMap[JSON_FS_INFO]) {
		return false
	}

	for _, fsinfo := range jsonMap[JSON_FS_INFO].([]interface{}) {
		if !utils.IsStringAnyMap(fsinfo) {
			continue
		}
		info := fsinfo.(map[string]interface{})
		if !utils.IsFloat64(info[JSON_MOUNT_NUM]) ||
			int(info[JSON_MOUNT_NUM].(float64)) <= 0 ||
			!utils.IsAnySlice(info[JSON_MOUNTPOINTS].([]interface{})) {
			continue
		}
		for _, mountpoint := range info[JSON_MOUNTPOINTS].([]interface{}) {
			if !utils.IsStringAnyMap(mountpoint) {
				continue
			}
			point := mountpoint.(map[string]interface{})
			if point[JSON_HOSTNAME] == hostname && point[JSON_PATH] == path {
				return true
			}
		}
	}

	return false
}
