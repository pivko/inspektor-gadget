package containerutils

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	ocispec "github.com/opencontainers/runtime-spec/specs-go"

	"google.golang.org/grpc"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

/*
#define _GNU_SOURCE
#include <stdlib.h>
#include <stdio.h>
#include <sys/types.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <stdint.h>

struct cgid_file_handle
{
  //struct file_handle handle;
  unsigned int handle_bytes;
  int handle_type;
  uint64_t cgid;
};

uint64_t get_cgroupid(char *path) {
  struct cgid_file_handle *h;
  int mount_id;
  int err;
  uint64_t ret;

  h = malloc(sizeof(struct cgid_file_handle));
  if (!h)
    return 0;

  h->handle_bytes = 8;
  err = name_to_handle_at(AT_FDCWD, path, (struct file_handle *)h, &mount_id, 0);
  if (err != 0)
    return 0;

  if (h->handle_bytes != 8)
    return 0;

  ret = h->cgid;
  free(h);

  return ret;
}
*/
import "C"

const (
	CONTAINERD_DEFAULT_SOCKET_PATH  = "/run/containerd/containerd.sock"
	CRIO_DEFAULT_SOCKET_PATH        = "/run/crio/crio.sock"
	DOCKER_SHIM_DEFAULT_SOCKER_PATH = "/var/run/dockershim.sock"
)

func CgroupPathV2AddMountpoint(path string) (string, error) {
	pathWithMountpoint := filepath.Join("/sys/fs/cgroup/unified", path)
	if _, err := os.Stat(pathWithMountpoint); os.IsNotExist(err) {
		pathWithMountpoint = filepath.Join("/sys/fs/cgroup", path)
		if _, err := os.Stat(pathWithMountpoint); os.IsNotExist(err) {
			return "", fmt.Errorf("cannot access cgroup %q: %v", path, err)
		}
	}
	return pathWithMountpoint, nil
}

// GetCgroupID returns the cgroup2 ID of a path.
func GetCgroupID(pathWithMountpoint string) (uint64, error) {
	cPathWithMountpoint := C.CString(pathWithMountpoint)
	ret := uint64(C.get_cgroupid(cPathWithMountpoint))
	C.free(unsafe.Pointer(cPathWithMountpoint))
	if ret == 0 {
		return 0, fmt.Errorf("GetCgroupID on %q failed", pathWithMountpoint)
	}
	return ret, nil
}

// GetCgroup2Path returns the cgroup1 and cgroup2 paths of a process.
// It does not include the "/sys/fs/cgroup/{unified,systemd,}" prefix.
func GetCgroupPaths(pid int) (string, string, error) {
	cgroupPathV1 := ""
	cgroupPathV2 := ""
	if cgroupFile, err := os.Open(filepath.Join("/proc", fmt.Sprintf("%d", pid), "cgroup")); err == nil {
		defer cgroupFile.Close()
		reader := bufio.NewReader(cgroupFile)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			if strings.HasPrefix(line, "1:name=systemd:") {
				cgroupPathV1 = strings.TrimPrefix(line, "1:name=systemd:")
				cgroupPathV1 = strings.TrimSuffix(cgroupPathV1, "\n")
				continue
			}
			if strings.HasPrefix(line, "0::") {
				cgroupPathV2 = strings.TrimPrefix(line, "0::")
				cgroupPathV2 = strings.TrimSuffix(cgroupPathV2, "\n")
				continue
			}
		}
	} else {
		return "", "", fmt.Errorf("cannot parse cgroup: %v", err)
	}

	if cgroupPathV1 == "/" {
		cgroupPathV1 = ""
	}

	if cgroupPathV2 == "/" {
		cgroupPathV2 = ""
	}

	if cgroupPathV2 == "" && cgroupPathV1 == "" {
		return "", "", fmt.Errorf("cannot find cgroup path in /proc/PID/cgroup")
	}

	return cgroupPathV1, cgroupPathV2, nil
}

func GetMntNs(pid int) (uint64, error) {
	fileinfo, err := os.Stat(filepath.Join("/proc", fmt.Sprintf("%d", pid), "ns/mnt"))
	if err != nil {
		return 0, err
	}
	stat, ok := fileinfo.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("Not a syscall.Stat_t")
	}
	return stat.Ino, nil
}

func PidFromContainerId(containerID string) (int, error) {
	if strings.HasPrefix(containerID, "docker://") {
		out, err := exec.Command("chroot", "/host", "docker", "inspect", strings.TrimPrefix(containerID, "docker://")).Output()
		if err != nil {
			return -1, err
		}
		type DockerInspect struct {
			State struct {
				Pid int
			}
		}
		var dockerInspect []DockerInspect
		err = json.Unmarshal(out, &dockerInspect)
		if err != nil {
			return -1, err
		}
		if len(dockerInspect) != 1 {
			return -1, fmt.Errorf("invalid output")
		}
		return dockerInspect[0].State.Pid, nil
	} else if strings.HasPrefix(containerID, "cri-o://") {
		IDWithoutPrefix := strings.TrimPrefix(containerID, "cri-o://")
		r, err := getContainerStatus(CRIO_DEFAULT_SOCKET_PATH, IDWithoutPrefix)
		if err != nil {
			return -1, err
		}
		pidStr, ok := r.Info["pid"]
		if !ok {
			return -1, fmt.Errorf("container status reply from runtime doesn't contain 'pid'")
		}

		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			return -1, err
		}

		return pid, nil
	} else if strings.HasPrefix(containerID, "containerd://") {
		IDWithoutPrefix := strings.TrimPrefix(containerID, "containerd://")
		r, err := getContainerStatus(CONTAINERD_DEFAULT_SOCKET_PATH, IDWithoutPrefix)
		if err != nil {
			return -1, err
		}
		info, ok := r.Info["info"]
		if !ok {
			return -1, fmt.Errorf("container status reply from runtime doesn't contain 'info'")
		}

		containerdInspect := struct{ Pid int }{}
		if err := json.Unmarshal([]byte(info), &containerdInspect); err != nil {
			return -1, err
		}

		if containerdInspect.Pid == 0 {
			return -1, fmt.Errorf("invalid pid")
		}
		return containerdInspect.Pid, nil
	}
	return -1, fmt.Errorf("unknown container runtime: %s", containerID)
}

func getContainerStatus(sockPath string, containerdID string) (*pb.ContainerStatusResponse, error) {
	conn, err := getConnection(sockPath)
	if err != nil {
		return nil, err
	}

	runtimeClient := pb.NewRuntimeServiceClient(conn)

	request := &pb.ContainerStatusRequest{
		ContainerId: containerdID,
		Verbose:     true,
	}

	return runtimeClient.ContainerStatus(context.Background(), request)
}

func getConnection(path string) (*grpc.ClientConn, error) {
	return grpc.Dial(
		path,
		grpc.WithInsecure(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", path, 2*time.Second)
		}))
}

func ParseOCIState(stateBuf []byte) (id string, pid int, err error) {
	ociState := &ocispec.State{}
	err = json.Unmarshal(stateBuf, ociState)
	if err != nil {
		// Some versions of runc produce an invalid json...
		// As a workaround, make it valid by trimming the invalid parts
		fix := regexp.MustCompile(`(?ms)^(.*),"annotations":.*$`)
		matches := fix.FindStringSubmatch(string(stateBuf))
		if len(matches) != 2 {
			err = fmt.Errorf("cannot parse OCI state: matches=%+v\n %v\n%s\n", matches, err, string(stateBuf))
			return
		}
		err = json.Unmarshal([]byte(matches[1]+"}"), ociState)
		if err != nil {
			err = fmt.Errorf("cannot parse OCI state: %v\n%s\n", err, string(stateBuf))
			return
		}
	}
	id = ociState.ID
	pid = ociState.Pid
	return
}
