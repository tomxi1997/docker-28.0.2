package fs

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fscommon"
	"github.com/opencontainers/runc/libcontainer/configs"
)

type CpusetGroup struct{}

func (s *CpusetGroup) Name() string {
	return "cpuset"
}

func (s *CpusetGroup) Apply(path string, r *configs.Resources, pid int) error {
	return s.ApplyDir(path, r, pid)
}

func (s *CpusetGroup) Set(path string, r *configs.Resources) error {
	if r.CpusetCpus != "" {
		if err := cgroups.WriteFile(path, "cpus", r.CpusetCpus); err != nil {
			return err
		}
	}
	if r.CpusetMems != "" {
		if err := cgroups.WriteFile(path, "mems", r.CpusetMems); err != nil {
			return err
		}
	}
	return nil
}

func getCpusetStat(path string, file string) ([]uint16, error) {
	var extracted []uint16
	fileContent, err := fscommon.GetCgroupParamString(path, file)
	if err != nil {
		return extracted, err
	}
	if len(fileContent) == 0 {
		return extracted, &parseError{Path: path, File: file, Err: errors.New("empty file")}
	}

	for _, s := range strings.Split(fileContent, ",") {
		sp := strings.SplitN(s, "-", 3)
		switch len(sp) {
		case 3:
			return extracted, &parseError{Path: path, File: file, Err: errors.New("extra dash")}
		case 2:
			min, err := strconv.ParseUint(sp[0], 10, 16)
			if err != nil {
				return extracted, &parseError{Path: path, File: file, Err: err}
			}
			max, err := strconv.ParseUint(sp[1], 10, 16)
			if err != nil {
				return extracted, &parseError{Path: path, File: file, Err: err}
			}
			if min > max {
				return extracted, &parseError{Path: path, File: file, Err: errors.New("invalid values, min > max")}
			}
			for i := min; i <= max; i++ {
				extracted = append(extracted, uint16(i))
			}
		case 1:
			value, err := strconv.ParseUint(s, 10, 16)
			if err != nil {
				return extracted, &parseError{Path: path, File: file, Err: err}
			}
			extracted = append(extracted, uint16(value))
		}
	}

	return extracted, nil
}

func (s *CpusetGroup) GetStats(path string, stats *cgroups.Stats) error {
	var err error

	stats.CPUSetStats.CPUs, err = getCpusetStat(path, "cpus")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.CPUExclusive, err = fscommon.GetCgroupParamUint(path, "cpuset.cpu_exclusive")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.Mems, err = getCpusetStat(path, "mems")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.MemHardwall, err = fscommon.GetCgroupParamUint(path, "cpuset.mem_hardwall")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.MemExclusive, err = fscommon.GetCgroupParamUint(path, "cpuset.mem_exclusive")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.MemoryMigrate, err = fscommon.GetCgroupParamUint(path, "cpuset.memory_migrate")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.MemorySpreadPage, err = fscommon.GetCgroupParamUint(path, "cpuset.memory_spread_page")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.MemorySpreadSlab, err = fscommon.GetCgroupParamUint(path, "cpuset.memory_spread_slab")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.MemoryPressure, err = fscommon.GetCgroupParamUint(path, "cpuset.memory_pressure")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.SchedLoadBalance, err = fscommon.GetCgroupParamUint(path, "cpuset.sched_load_balance")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stats.CPUSetStats.SchedRelaxDomainLevel, err = fscommon.GetCgroupParamInt(path, "cpuset.sched_relax_domain_level")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func (s *CpusetGroup) ApplyDir(dir string, r *configs.Resources, pid int) error {
	// This might happen if we have no cpuset cgroup mounted.
	// Just do nothing and don't fail.
	if dir == "" {
		return nil
	}
	// 'ensureParent' start with parent because we don't want to
	// explicitly inherit from parent, it could conflict with
	// 'cpuset.cpu_exclusive'.
	if err := cpusetEnsureParent(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	// We didn't inherit cpuset configs from parent, but we have
	// to ensure cpuset configs are set before moving task into the
	// cgroup.
	// The logic is, if user specified cpuset configs, use these
	// specified configs, otherwise, inherit from parent. This makes
	// cpuset configs work correctly with 'cpuset.cpu_exclusive', and
	// keep backward compatibility.
	if err := s.ensureCpusAndMems(dir, r); err != nil {
		return err
	}
	// Since we are not using apply(), we need to place the pid
	// into the procs file.
	return cgroups.WriteCgroupProc(dir, pid)
}

func getCpusetSubsystemSettings(parent string) (cpus, mems string, err error) {
	if cpus, err = cgroups.ReadFile(parent, "cpus"); err != nil {
		return
	}
	if mems, err = cgroups.ReadFile(parent, "mems"); err != nil {
		return
	}
	return cpus, mems, nil
}

// cpusetEnsureParent makes sure that the parent directories of current
// are created and populated with the proper cpus and mems files copied
// from their respective parent. It does that recursively, starting from
// the top of the cpuset hierarchy (i.e. cpuset cgroup mount point).
func cpusetEnsureParent(current string) error {
	var st unix.Statfs_t

	parent := filepath.Dir(current)
	err := unix.Statfs(parent, &st)
	if err == nil && st.Type != unix.CGROUP_SUPER_MAGIC {
		return nil
	}
	// Treat non-existing directory as cgroupfs as it will be created,
	// and the root cpuset directory obviously exists.
	if err != nil && err != unix.ENOENT {
		return &os.PathError{Op: "statfs", Path: parent, Err: err}
	}

	if err := cpusetEnsureParent(parent); err != nil {
		return err
	}
	if err := os.Mkdir(current, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return cpusetCopyIfNeeded(current, parent)
}

// cpusetCopyIfNeeded copies the cpuset.cpus and cpuset.mems from the parent
// directory to the current directory if the file's contents are 0
func cpusetCopyIfNeeded(current, parent string) error {
	currentCpus, currentMems, err := getCpusetSubsystemSettings(current)
	if err != nil {
		return err
	}
	parentCpus, parentMems, err := getCpusetSubsystemSettings(parent)
	if err != nil {
		return err
	}

	if isEmptyCpuset(currentCpus) {
		if err := cgroups.WriteFile(current, "cpus", parentCpus); err != nil {
			return err
		}
	}
	if isEmptyCpuset(currentMems) {
		if err := cgroups.WriteFile(current, "mems", parentMems); err != nil {
			return err
		}
	}
	return nil
}

func isEmptyCpuset(str string) bool {
	return str == "" || str == "\n"
}

func (s *CpusetGroup) ensureCpusAndMems(path string, r *configs.Resources) error {
	if err := s.Set(path, r); err != nil {
		return err
	}
	return cpusetCopyIfNeeded(path, filepath.Dir(path))
}
