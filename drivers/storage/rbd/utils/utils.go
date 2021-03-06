// +build !libstorage_storage_driver libstorage_storage_driver_rbd

package utils

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/akutz/goof"

	"github.com/codedellemc/libstorage/api/types"
)

const (
	radosCmd  = "rados"
	rbdCmd    = "rbd"
	formatOpt = "--format"
	jsonArg   = "json"
	poolOpt   = "--pool"

	bytesPerGiB = 1024 * 1024 * 1024
)

type rbdMappedEntry struct {
	Device string `json:"device"`
	Name   string `json:"name"`
	Pool   string `json:"pool"`
	Snap   string `json:"snap"`
}

//RBDImage holds details about an RBD image
type RBDImage struct {
	Name   string `json:"image"`
	Size   int64  `json:"size"`
	Format uint   `json:"format"`
	Pool   string
}

//RBDInfo holds low-level details about an RBD image
type RBDInfo struct {
	Name            string   `json:"name"`
	Size            int64    `json:"size"`
	Objects         int64    `json:"objects"`
	Order           int64    `json:"order"`
	ObjectSize      int64    `json:"object_size"`
	BlockNamePrefix string   `json:"block_name_prefix"`
	Format          int64    `json:"format"`
	Features        []string `json:"features"`
	Pool            string
}

//GetRadosPools returns a slice containing all the pool names
func GetRadosPools(ctx types.Context) ([]*string, error) {

	cmd := exec.Command(radosCmd, "lspools")
	ctx.WithFields(map[string]interface{}{
		"cmd":  radosCmd,
		"args": cmd.Args,
	}).Debug("running command")

	out, err := cmd.Output()
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			stderr := string(exiterr.Stderr)
			ctx.WithError(
				exiterr,
			).WithField(
				"stderr", stderr,
			).Error("Unable to get pools")
			return nil,
				goof.Newf("Unable to get pools: %s", stderr)
		}
		return nil, goof.WithError("Unable to get pools", err)
	}

	var pools []string

	rdr := bytes.NewReader(out)
	scanner := bufio.NewScanner(rdr)

	for scanner.Scan() {
		pools = append(pools, scanner.Text())
	}

	return ConvStrArrayToPtr(pools), nil
}

//GetRBDImages returns a slice of RBD image info
func GetRBDImages(ctx types.Context, pool *string) ([]*RBDImage, error) {

	cmd := exec.Command(rbdCmd, "ls", "-p", *pool, "-l", formatOpt, jsonArg)
	ctx.WithFields(map[string]interface{}{
		"cmd":  rbdCmd,
		"args": cmd.Args,
	}).Debug("running command")

	out, err := cmd.Output()
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			stderr := string(exiterr.Stderr)
			ctx.WithError(
				exiterr,
			).WithField(
				"stderr", stderr,
			).Error("Unable to get rbd images")
			return nil,
				goof.Newf("Unable to get rbd images: %s",
					stderr)
		}
		return nil, goof.WithError("Unable to get rbd images", err)
	}

	var rbdList []*RBDImage

	err = json.Unmarshal(out, &rbdList)
	if err != nil {
		return nil, goof.WithError(
			"Unable to parse rbd ls", err)
	}

	for _, info := range rbdList {
		info.Pool = *pool
	}

	return rbdList, nil
}

//GetRBDInfo gets low-level details about an RBD image
func GetRBDInfo(
	ctx types.Context,
	pool *string,
	name *string) (*RBDInfo, error) {

	cmd := exec.Command(
		rbdCmd, "info", "-p", *pool, *name, formatOpt, jsonArg)

	ctx.WithFields(map[string]interface{}{
		"cmd":  rbdCmd,
		"args": cmd.Args,
	}).Debug("running command")

	out, err := cmd.Output()

	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				if status.ExitStatus() == 2 {
					// image does not exist
					return nil, nil
				}
			}
			stderr := string(exiterr.Stderr)
			ctx.WithError(
				exiterr,
			).WithField(
				"stderr", stderr,
			).Error("Unable to get rbd info")
			return nil,
				goof.Newf("Unable to get rbd info: %s",
					stderr)
		}
		return nil, goof.WithError("Unable to get rbd info", err)
	}

	info := &RBDInfo{}

	err = json.Unmarshal(out, info)
	if err != nil {
		return nil, goof.WithError(
			"Unable to parse rbd info", err)
	}

	info.Pool = *pool

	return info, nil
}

//GetVolumeID returns an RBD Volume formatted as <pool>.<imageName>
func GetVolumeID(pool, image *string) *string {

	volumeID := fmt.Sprintf("%s.%s", *pool, *image)
	return &volumeID
}

//GetMappedRBDs returns a map of RBDs currently mapped to the *local* host
func GetMappedRBDs(ctx types.Context) (map[string]string, error) {

	cmd := exec.Command(
		rbdCmd, "showmapped", formatOpt, jsonArg)
	ctx.WithFields(map[string]interface{}{
		"cmd":  rbdCmd,
		"args": cmd.Args,
	}).Debug("running command")

	out, err := cmd.Output()

	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			stderr := string(exiterr.Stderr)
			ctx.WithError(
				exiterr,
			).WithField(
				"stderr", stderr,
			).Error("Unable to get rbd map")
			return nil,
				goof.Newf("Unable to get RBD map: %s",
					stderr)
		}
		return nil, goof.WithError("Unable to get RBD map", err)
	}

	devMap := map[string]string{}
	rbdMap := map[string]*rbdMappedEntry{}

	err = json.Unmarshal(out, &rbdMap)
	if err != nil {
		return nil, goof.WithError(
			"Unable to parse rbd showmapped", err)
	}

	for _, mapped := range rbdMap {
		volumeID := GetVolumeID(&mapped.Pool, &mapped.Name)
		devMap[*volumeID] = mapped.Device
	}

	return devMap, nil
}

//RBDCreate creates a new RBD volume on the cluster
func RBDCreate(
	ctx types.Context,
	pool *string,
	image *string,
	sizeGB *int64,
	objectSize *string,
	features []*string) error {

	cmd := exec.Command(
		rbdCmd, "create", poolOpt, *pool,
		"--object-size", *objectSize,
		"--size", strconv.FormatInt(*sizeGB, 10)+"G",
	)

	for _, feature := range features {
		cmd.Args = append(cmd.Args, "--image-feature")
		cmd.Args = append(cmd.Args, *feature)
	}

	cmd.Args = append(cmd.Args, *image)
	ctx.WithFields(map[string]interface{}{
		"cmd":  rbdCmd,
		"args": cmd.Args,
	}).Debug("running command")

	err := cmd.Run()

	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			stderr := string(exiterr.Stderr)
			ctx.WithError(
				exiterr,
			).WithField(
				"stderr", stderr,
			).Error("Unable to create RBD")
			return goof.Newf("Unable to create RBD: %s",
				stderr)
		}
		return goof.WithError("Unable to create RBD", err)
	}

	return nil
}

//RBDRemove deletes the RBD volume on the cluster
func RBDRemove(ctx types.Context, pool *string, image *string) error {
	cmd := exec.Command(rbdCmd, "rm", poolOpt, *pool, "--no-progress",
		*image,
	)
	ctx.WithFields(map[string]interface{}{
		"cmd":  rbdCmd,
		"args": cmd.Args,
	}).Debug("running command")

	err := cmd.Run()
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			stderr := string(exiterr.Stderr)
			ctx.WithError(
				exiterr,
			).WithField(
				"stderr", stderr,
			).Error("Unable to delete RBD")
			return goof.Newf("Error deleting RBD: %s",
				stderr)
		}
		return goof.WithError("Error deleting RBD", err)
	}

	return nil
}

//RBDMap attaches the given RBD image to the *local* host
func RBDMap(ctx types.Context, pool, image *string) (string, error) {

	cmd := exec.Command(rbdCmd, "map", poolOpt, *pool, *image)
	ctx.WithFields(map[string]interface{}{
		"cmd":  rbdCmd,
		"args": cmd.Args,
	}).Debug("running command")

	out, err := cmd.Output()
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			stderr := string(exiterr.Stderr)
			ctx.WithError(
				exiterr,
			).WithField(
				"stderr", stderr,
			).Error("Unable to map RBD")
			return "",
				goof.Newf("Unable to map RBD: %s",
					stderr)
		}
		return "", goof.WithError("Unable to map RBD", err)
	}

	return strings.TrimSpace(string(out)), nil
}

//RBDUnmap detaches the given RBD device from the *local* host
func RBDUnmap(ctx types.Context, device *string) error {

	cmd := exec.Command(rbdCmd, "unmap", *device)
	ctx.WithFields(map[string]interface{}{
		"cmd":  rbdCmd,
		"args": cmd.Args,
	}).Debug("running command")

	err := cmd.Run()
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			stderr := string(exiterr.Stderr)
			ctx.WithError(
				exiterr,
			).WithField(
				"stderr", stderr,
			).Error("Unable to unmap RBD")
			return goof.Newf("Unable to unmap RBD: %s",
				stderr)
		}
		return goof.WithError("Unable to unmap RBD", err)
	}

	return nil
}

//GetRBDStatus returns a map of RBD status info
func GetRBDStatus(
	ctx types.Context,
	pool, image *string) (map[string]interface{}, error) {

	cmd := exec.Command(
		rbdCmd, "status", poolOpt, *pool, *image, formatOpt, jsonArg,
	)
	ctx.WithFields(map[string]interface{}{
		"cmd":  rbdCmd,
		"args": cmd.Args,
	}).Debug("running command")

	out, err := cmd.Output()
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			stderr := string(exiterr.Stderr)
			ctx.WithError(
				exiterr,
			).WithField(
				"stderr", stderr,
			).Error("Unable to get RBD status")
			return nil, goof.Newf("Unable to get RBD status: %s",
				stderr)
		}
		return nil, goof.WithError("Unable to get RBD status", err)
	}

	watcherMap := map[string]interface{}{}

	err = json.Unmarshal(out, &watcherMap)
	if err != nil {
		return nil, goof.WithError(
			"Unable to parse rbd status", err)
	}

	return watcherMap, nil
}

//RBDHasWatchers returns true if RBD image has watchers
func RBDHasWatchers(
	ctx types.Context,
	pool *string,
	image *string) (bool, error) {

	m, err := GetRBDStatus(ctx, pool, image)
	if err != nil {
		return false, err
	}

	/*  The "watchers" key can have two differently formatted values,
	    depending on Ceph version. Originally, it was a map:

	    {"watchers": {"watcher": ...}}

	    Later versions switched to an array:

	    {"watchers": [{}, {}, ...]}
	*/

	switch v := m["watchers"].(type) {
	case map[string]interface{}:
		return len(v) > 0, nil
	case []interface{}:
		return len(v) > 0, nil
	default:
		return false, goof.New("Unable to parse RBD status watchers")
	}
}

//ConvStrArrayToPtr converts the slice of strings to a slice of pointers to str
func ConvStrArrayToPtr(strArr []string) []*string {
	ptrArr := make([]*string, len(strArr))
	for i := range strArr {
		ptrArr[i] = &strArr[i]
	}
	return ptrArr
}

// ParseMonitorAddresses returns a slice of IP address from the given slice of
// string addresses. Addresses can be IPv4, IPv4:port, [IPv6], or [IPv6]:port
func ParseMonitorAddresses(addrs []string) ([]net.IP, error) {
	monIps := []net.IP{}

	var (
		host string
		err  error
	)

	for _, mon := range addrs {
		host = mon
		if hasPort(mon) {
			host, _, err = net.SplitHostPort(mon)
			if err != nil {
				return nil, err
			}
		}
		if strings.HasPrefix(host, "[") {
			// pull the host/IP out of the brackets
			host = strings.Trim(host, "[]")
		}
		ip := net.ParseIP(host)
		if ip != nil {
			monIps = append(monIps, ip)
		} else {
			ips, err := net.LookupIP(host)
			if err != nil {
				return nil, err
			}
			if len(ips) > 0 {
				monIps = append(monIps, ips...)
			}
		}
	}

	return monIps, nil
}

var ipv6wPortRX = regexp.MustCompile(`^\[.*\]:\d+$`)

func hasPort(addr string) bool {
	if strings.HasPrefix(addr, "[") {
		// IPv6
		return ipv6wPortRX.MatchString(addr)
	}
	return strings.Contains(addr, ":")
}
