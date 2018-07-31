// Copyright (c) 2018, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package singularity

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/singularityware/singularity/src/pkg/buildcfg"
	"github.com/singularityware/singularity/src/pkg/image"
	"github.com/singularityware/singularity/src/pkg/sylog"
	"github.com/singularityware/singularity/src/pkg/util/fs"
	"github.com/singularityware/singularity/src/pkg/util/fs/files"
	"github.com/singularityware/singularity/src/pkg/util/fs/layout"
	"github.com/singularityware/singularity/src/pkg/util/fs/layout/layer/overlay"
	"github.com/singularityware/singularity/src/pkg/util/fs/layout/layer/underlay"
	"github.com/singularityware/singularity/src/pkg/util/fs/mount"
	"github.com/singularityware/singularity/src/pkg/util/fs/proc"
	"github.com/singularityware/singularity/src/pkg/util/loop"
	"github.com/singularityware/singularity/src/pkg/util/user"
	"github.com/singularityware/singularity/src/runtime/engines/singularity/rpc/client"
	"github.com/sylabs/sif/pkg/sif"
)

type container struct {
	engine        *EngineOperations
	rpcOps        *client.RPC
	session       *layout.Session
	sessionFsType string
	sessionSize   int
	userNS        bool
	pidNS         bool
}

func create(engine *EngineOperations, rpcOps *client.RPC) error {
	var err error

	c := &container{engine: engine, rpcOps: rpcOps}

	c.sessionFsType = engine.EngineConfig.File.MemoryFSType
	if os.Geteuid() != 0 {
		c.sessionSize = int(engine.EngineConfig.File.SessiondirMaxSize)
	}

	if engine.CommonConfig.OciConfig.Linux != nil {
		for _, namespace := range engine.CommonConfig.OciConfig.Linux.Namespaces {
			switch namespace.Type {
			case specs.UserNamespace:
				c.userNS = true
			case specs.PIDNamespace:
				c.pidNS = true
			}
		}
	}

	p := &mount.Points{}
	system := &mount.System{Points: p, Mount: c.localMount}

	if err := c.setupSessionLayout(system); err != nil {
		return err
	}

	if err := system.RunAfterTag(mount.LayerTag, c.addFilesMount); err != nil {
		return err
	}

	if err := c.addRootfsMount(system); err != nil {
		return err
	}
	if err := c.addKernelMount(system); err != nil {
		return err
	}
	if err := c.addDevMount(system); err != nil {
		return err
	}
	if err := c.addHostMount(system); err != nil {
		return err
	}
	if err := c.addBindsMount(system); err != nil {
		return err
	}
	if err := c.addHomeMount(system); err != nil {
		return err
	}
	if err := c.addUserbindsMount(system); err != nil {
		return err
	}
	if err := c.addTmpMount(system); err != nil {
		return err
	}
	if err := c.addScratchMount(system); err != nil {
		return err
	}
	if err := c.addCwdMount(system); err != nil {
		return err
	}
	if err := c.addLibsMount(system); err != nil {
		return err
	}

	sylog.Debugf("Mount all")
	if err := system.MountAll(); err != nil {
		return err
	}

	sylog.Debugf("Chroot into %s\n", c.session.FinalPath())
	_, err = c.rpcOps.Chroot(c.session.FinalPath())
	if err != nil {
		return fmt.Errorf("chroot failed: %s", err)
	}

	sylog.Debugf("Chdir into / to avoid errors\n")
	err = syscall.Chdir("/")
	if err != nil {
		return fmt.Errorf("change directory failed: %s", err)
	}

	return nil
}

// setupSessionLayout will create the session layout according to the capabilities of Singularity
// on the system. It will first attempt to use "overlay", followed by "underlay", and if neither
// are available it will not use either. If neither are used, we will not be able to bind mount
// to non-existant paths within the container
func (c *container) setupSessionLayout(system *mount.System) error {
	if c.engine.EngineConfig.GetWritableImage() {
		sylog.Debugf("Image is writable, not attempting to use overlay or underlay\n")
		return c.setupDefaultLayout(system)
	}

	if enabled, _ := proc.HasFilesystem("overlay"); enabled && !c.userNS {
		switch c.engine.EngineConfig.File.EnableOverlay {
		case "yes", "try":
			sylog.Debugf("Attempting to use overlayfs (enable overlay = %v)\n", c.engine.EngineConfig.File.EnableOverlay)
			return c.setupOverlayLayout(system)
		}
	}

	if c.engine.EngineConfig.File.EnableUnderlay {
		sylog.Debugf("Attempting to use underlay (enable underlay = yes)\n")
		return c.setupUnderlayLayout(system)
	}

	sylog.Debugf("Not attempting to use underlay or overlay\n")
	return c.setupDefaultLayout(system)
}

// setupOverlayLayout sets up the session with overlay filesystem
func (c *container) setupOverlayLayout(system *mount.System) (err error) {
	sylog.Debugf("Creating overlay SESSIONDIR layout\n")
	if c.session, err = layout.NewSession(buildcfg.SESSIONDIR, c.sessionFsType, c.sessionSize, system, overlay.New()); err != nil {
		return err
	}

	if err := c.addOverlayMount(system); err != nil {
		return err
	}

	return system.RunAfterTag(mount.LayerTag, c.switchMount)
}

// setupUnderlayLayout sets up the session with underlay "filesystem"
func (c *container) setupUnderlayLayout(system *mount.System) (err error) {
	sylog.Debugf("Creating underlay SESSIONDIR layout\n")
	if c.session, err = layout.NewSession(buildcfg.SESSIONDIR, c.sessionFsType, c.sessionSize, system, underlay.New()); err != nil {
		return err
	}

	return system.RunAfterTag(mount.LayerTag, c.switchMount)
}

// setupDefaultLayout sets up the session without overlay or underlay
func (c *container) setupDefaultLayout(system *mount.System) (err error) {
	sylog.Debugf("Creating default SESSIONDIR layout\n")
	if c.session, err = layout.NewSession(buildcfg.SESSIONDIR, c.sessionFsType, c.sessionSize, system, nil); err != nil {
		return err
	}

	return system.RunAfterTag(mount.RootfsTag, c.switchMount)
}

func (c *container) localMount(point *mount.Point) error {
	if !c.userNS {
		uid := os.Getuid()

		if err := syscall.Setresuid(uid, 0, uid); err != nil {
			return fmt.Errorf("failed to elevate privileges")
		}
		defer syscall.Setresuid(uid, uid, 0)

		if err := syscall.Setfsuid(uid); err != nil {
			return fmt.Errorf("failed to set FS uid")
		}
	}

	if _, err := mount.GetOffset(point.InternalOptions); err == nil {
		if err := c.mountImage(point); err != nil {
			return fmt.Errorf("can't mount image %s: %s", point.Source, err)
		}
	} else {
		if err := c.mountGeneric(point, true); err != nil {
			flags, _ := mount.ConvertOptions(point.Options)
			if flags&syscall.MS_REMOUNT != 0 {
				return fmt.Errorf("can't remount %s: %s", point.Destination, err)
			}
			sylog.Verbosef("can't mount %s: %s", point.Source, err)
			return nil
		}
	}
	return nil
}

func (c *container) rpcMount(point *mount.Point) error {
	if err := c.mountGeneric(point, false); err != nil {
		flags, _ := mount.ConvertOptions(point.Options)
		if flags&syscall.MS_REMOUNT != 0 {
			return fmt.Errorf("can't remount %s: %s", point.Destination, err)
		}
		sylog.Verbosef("can't mount %s: %s", point.Source, err)
		return nil
	}
	return nil
}

func (c *container) switchMount(system *mount.System) error {
	system.Mount = c.rpcMount
	return nil
}

// mount any generic mount (not loop dev)
func (c *container) mountGeneric(mnt *mount.Point, local bool) (err error) {
	flags, opts := mount.ConvertOptions(mnt.Options)
	optsString := strings.Join(opts, ",")
	sessionPath := c.session.Path()
	remount := false

	if flags&syscall.MS_REMOUNT != 0 {
		remount = true
	}

	if flags&syscall.MS_BIND != 0 && !remount {
		if _, err := os.Stat(mnt.Source); os.IsNotExist(err) {
			sylog.Debugf("Skipping mount, host source %s doesn't exist", mnt.Source)
			return nil
		}
	}

	dest := ""
	if !strings.HasPrefix(mnt.Destination, sessionPath) {
		dest = c.session.FinalPath() + mnt.Destination
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			sylog.Debugf("Skipping mount, %s doesn't exist in container", dest)
			return nil
		}
	} else {
		dest = mnt.Destination
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			return fmt.Errorf("destination %s doesn't exist", dest)
		}
	}

	if remount {
		sylog.Debugf("Remounting %s\n", dest)
	} else {
		sylog.Debugf("Mounting %s to %s\n", mnt.Source, dest)
	}
	if !local {
		_, err = c.rpcOps.Mount(mnt.Source, dest, mnt.Type, flags, optsString)
	} else {
		err = syscall.Mount(mnt.Source, dest, mnt.Type, flags, optsString)
	}
	return err
}

// mount image via loop
func (c *container) mountImage(mnt *mount.Point) error {
	flags, opts := mount.ConvertOptions(mnt.Options)
	optsString := strings.Join(opts, ",")

	offset, err := mount.GetOffset(mnt.InternalOptions)
	if err != nil {
		return err
	}

	sizelimit, err := mount.GetSizeLimit(mnt.InternalOptions)
	if err != nil {
		return err
	}

	attachFlag := os.O_RDWR
	loopFlags := uint32(loop.FlagsAutoClear)

	if flags&syscall.MS_RDONLY == 1 {
		loopFlags |= loop.FlagsReadOnly
		attachFlag = os.O_RDONLY
	}

	info := &loop.Info64{
		Offset:    offset,
		SizeLimit: sizelimit,
		Flags:     loopFlags,
	}

	loopdev := new(loop.Device)

	number := 0

	if err := loopdev.Attach(mnt.Source, attachFlag, &number); err != nil {
		return err
	}
	if err := loopdev.SetStatus(info); err != nil {
		return err
	}

	path := fmt.Sprintf("/dev/loop%d", number)
	sylog.Debugf("Mounting loop device %s to %s\n", path, mnt.Destination)
	err = syscall.Mount(path, mnt.Destination, mnt.Type, flags, optsString)
	if err != nil {
		return fmt.Errorf("failed to mount %s filesystem: %s", mnt.Type, err)
	}

	return nil
}

func (c *container) loadImage(path string, writable bool) (*image.Image, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if fi.IsDir() {
		writable = false
	}

	imgObject, err := image.Init(path, writable)
	if err != nil {
		return nil, err
	}

	if len(c.engine.EngineConfig.File.LimitContainerPaths) != 0 {
		if authorized, err := imgObject.AuthorizedPath(c.engine.EngineConfig.File.LimitContainerPaths); err != nil {
			return nil, err
		} else if !authorized {
			return nil, fmt.Errorf("Singularity image is not an allowed configured path")
		}
	}
	if len(c.engine.EngineConfig.File.LimitContainerGroups) != 0 {
		if authorized, err := imgObject.AuthorizedGroup(c.engine.EngineConfig.File.LimitContainerGroups); err != nil {
			return nil, err
		} else if !authorized {
			return nil, fmt.Errorf("Singularity image is not owned by required group(s)")
		}
	}
	if len(c.engine.EngineConfig.File.LimitContainerOwners) != 0 {
		if authorized, err := imgObject.AuthorizedOwner(c.engine.EngineConfig.File.LimitContainerOwners); err != nil {
			return nil, err
		} else if !authorized {
			return nil, fmt.Errorf("Singularity image is not owned by required user(s)")
		}
	}
	switch imgObject.Type {
	case image.SANDBOX:
		if !c.engine.EngineConfig.File.AllowContainerDir {
			return nil, fmt.Errorf("configuration disallows users from running sandbox based containers")
		}
	case image.EXT3:
		if !c.engine.EngineConfig.File.AllowContainerExtfs {
			return nil, fmt.Errorf("configuration disallows users from running extFS based containers")
		}
	case image.SQUASHFS:
		if !c.engine.EngineConfig.File.AllowContainerSquashfs {
			return nil, fmt.Errorf("configuration disallows users from running squashFS based containers")
		}
	}
	return imgObject, nil
}

func (c *container) addRootfsMount(system *mount.System) error {
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
	rootfs := c.engine.EngineConfig.GetImage()

	imageObject, err := c.loadImage(rootfs, false)
	if err != nil {
		return err
	}

	mountType := ""

	switch imageObject.Type {
	case image.SIF:
		// Load the SIF file
		fimg, err := sif.LoadContainerFp(imageObject.File, !imageObject.Writable)
		if err != nil {
			return err
		}

		// Get the default system partition image
		part, _, err := fimg.GetPartFromGroup(sif.DescrDefaultGroup)
		if err != nil {
			return err
		}

		// Check that this is a system partition
		parttype, err := part.GetPartType()
		if err != nil {
			return err
		}
		if parttype != sif.PartSystem {
			return fmt.Errorf("found partition is not system")
		}

		// record the fs type
		fstype, err := part.GetFsType()
		if err != nil {
			return err
		}
		if fstype == sif.FsSquash {
			mountType = "squashfs"
		} else if fstype == sif.FsExt3 {
			mountType = "ext3"
		} else {
			return fmt.Errorf("unknown file system type: %v", fstype)
		}

		imageObject.Offset = uint64(part.Fileoff)
		imageObject.Size = uint64(part.Filelen)
	case image.SQUASHFS:
		mountType = "squashfs"
	case image.EXT3:
		mountType = "ext3"
	case image.SANDBOX:
		sylog.Debugf("Mounting directory rootfs: %v\n", rootfs)
		flags |= syscall.MS_BIND
		if err := system.Points.AddBind(mount.RootfsTag, rootfs, c.session.RootFsPath(), flags); err != nil {
			return err
		}
		// remount sandbox in PreLayerTag to apply flags in container namespace
		system.Points.AddRemount(mount.PreLayerTag, c.session.RootFsPath(), flags)
		return nil
	}
	flags |= syscall.MS_RDONLY

	src := fmt.Sprintf("/proc/self/fd/%d", imageObject.File.Fd())
	sylog.Debugf("Mounting block [%v] image: %v\n", mountType, rootfs)
	return system.Points.AddImage(mount.RootfsTag, src, c.session.RootFsPath(), mountType, flags, imageObject.Offset, imageObject.Size)
}

func (c *container) overlayUpperWork(system *mount.System) error {
	ov := c.session.Layer.(*overlay.Overlay)
	var point mount.Point

	for _, p := range system.Points.GetByTag(mount.PreLayerTag) {
		if p.Type == "ext3" || (p.Source != "" && p.Destination != "" && p.Type == "") {
			point = p
			break
		}
	}

	u := point.Destination + "/upper"
	w := point.Destination + "/work"

	if fs.IsLink(u) {
		return fmt.Errorf("symlink detected, upper overlay %s must be a directory", u)
	}
	if fs.IsLink(w) {
		return fmt.Errorf("symlink detected, work overlay %s must be a directory", w)
	}
	if !fs.IsDir(u) {
		if err := fs.MkdirAll(u, 0755); err != nil {
			return fmt.Errorf("failed to create %s directory: %s", u, err)
		}
	}
	if !fs.IsDir(w) {
		if err := fs.MkdirAll(w, 0755); err != nil {
			return fmt.Errorf("failed to create %s directory: %s", w, err)
		}
	}
	if err := ov.AddUpperDir(u); err != nil {
		return fmt.Errorf("failed to add overlay upper: %s", err)
	}
	if err := ov.AddWorkDir(w); err != nil {
		return fmt.Errorf("failed to add overlay upper: %s", err)
	}
	return nil
}

func (c *container) addOverlayMount(system *mount.System) error {
	nb := 0
	ov := c.session.Layer.(*overlay.Overlay)

	for _, img := range c.engine.EngineConfig.GetOverlayImage() {
		overlayImg := img
		writable := true

		splitted := strings.SplitN(img, ":", 2)
		if len(splitted) == 2 {
			if splitted[1] == "ro" {
				writable = false
				overlayImg = splitted[0]
			}
		}

		imageObject, err := c.loadImage(overlayImg, writable)
		if err != nil {
			return fmt.Errorf("failed to open overlay image %s: %s", overlayImg, err)
		}

		sessionDest := fmt.Sprintf("/overlay-images/%d", nb)
		if err := c.session.AddDir(sessionDest); err != nil {
			return fmt.Errorf("failed to create session directory for overlay: %s", err)
		}
		dst, _ := c.session.GetPath(sessionDest)
		nb++

		src := fmt.Sprintf("/proc/self/fd/%d", imageObject.File.Fd())
		switch imageObject.Type {
		case image.EXT3:
			flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
			err = system.Points.AddImage(mount.PreLayerTag, src, dst, "ext3", flags, imageObject.Offset, imageObject.Size)
			if err != nil {
				return err
			}
			if writable {
				if err := system.RunAfterTag(mount.PreLayerTag, c.overlayUpperWork); err != nil {
					return err
				}
			} else {
				ov.AddLowerDir(dst)
			}
		case image.SQUASHFS:
			flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_RDONLY)
			err = system.Points.AddImage(mount.PreLayerTag, src, dst, "squashfs", flags, imageObject.Offset, imageObject.Size)
			if err != nil {
				return err
			}
			if writable {
				sylog.Warningf("squashfs is not a writable filesystem")
			}
			ov.AddLowerDir(dst)
		case image.SANDBOX:
			if os.Geteuid() != 0 {
				return fmt.Errorf("only root user can use sandbox as overlay")
			}
			flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
			err = system.Points.AddBind(mount.PreLayerTag, src, dst, flags)
			if err != nil {
				return err
			}
			system.Points.AddRemount(mount.PreLayerTag, dst, flags)

			if writable {
				if err := system.RunAfterTag(mount.PreLayerTag, c.overlayUpperWork); err != nil {
					return err
				}
			} else {
				ov.AddLowerDir(dst)
			}
		default:
			return fmt.Errorf("unkown image format")
		}
	}
	return nil
}

func (c *container) addKernelMount(system *mount.System) error {
	var err error
	bindFlags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)

	sylog.Debugf("Checking configuration file for 'mount proc'")
	if c.engine.EngineConfig.File.MountProc {
		sylog.Debugf("Adding proc to mount list\n")
		if c.pidNS {
			err = system.Points.AddFS(mount.KernelTag, "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV, "")
		} else {
			err = system.Points.AddBind(mount.KernelTag, "/proc", "/proc", bindFlags)
			if err == nil {
				if !c.userNS {
					system.Points.AddRemount(mount.KernelTag, "/proc", bindFlags)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("unable to add proc to mount list: %s", err)
		}
	} else {
		sylog.Verbosef("Skipping /proc mount")
	}

	sylog.Debugf("Checking configuration file for 'mount sys'")
	if c.engine.EngineConfig.File.MountSys {
		sylog.Debugf("Adding sysfs to mount list\n")
		if !c.userNS {
			err = system.Points.AddFS(mount.KernelTag, "/sys", "sysfs", syscall.MS_NOSUID|syscall.MS_NODEV, "")
		} else {
			err = system.Points.AddBind(mount.KernelTag, "/sys", "/sys", bindFlags)
			if err == nil {
				if !c.userNS {
					system.Points.AddRemount(mount.KernelTag, "/sys", bindFlags)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("unable to add sys to mount list: %s", err)
		}
	} else {
		sylog.Verbosef("Skipping /sys mount")
	}
	return nil
}

func (c *container) bindDev(devpath string, system *mount.System) error {
	if err := c.session.AddFile(devpath, nil); err != nil {
		return fmt.Errorf("failed to %s session file: %s", devpath, err)
	}

	dst, _ := c.session.GetPath(devpath)

	sylog.Debugf("Mounting device %s at %s", devpath, dst)

	if err := system.Points.AddBind(mount.DevTag, devpath, dst, syscall.MS_BIND); err != nil {
		return fmt.Errorf("failed to add %s mount: %s", devpath, err)
	}
	return nil
}

func (c *container) addDevMount(system *mount.System) error {
	sylog.Debugf("Checking configuration file for 'mount dev'")

	if c.engine.EngineConfig.File.MountDev == "minimal" || c.engine.EngineConfig.GetContain() {
		sylog.Debugf("Creating temporary staged /dev")
		if err := c.session.AddDir("/dev"); err != nil {
			return fmt.Errorf("failed to add /dev session directory: %s", err)
		}
		sylog.Debugf("Creating temporary staged /dev/shm")
		if err := c.session.AddDir("/dev/shm"); err != nil {
			return fmt.Errorf("failed to add /dev/shm session directory: %s", err)
		}
		devshmPath, _ := c.session.GetPath("/dev/shm")
		flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
		err := system.Points.AddFS(mount.DevTag, devshmPath, c.engine.EngineConfig.File.MemoryFSType, flags, "mode=1777")
		if err != nil {
			return fmt.Errorf("failed to add /dev/shm temporary filesystem: %s", err)
		}
		if c.engine.EngineConfig.File.MountDevPts {
			if _, err := os.Stat("/dev/pts/ptmx"); os.IsNotExist(err) {
				return fmt.Errorf("Multiple devpts instances unsupported and /dev/pts configured")
			}

			sylog.Debugf("Creating temporary staged /dev/pts")
			if err := c.session.AddDir("/dev/pts"); err != nil {
				return fmt.Errorf("failed to /dev/pts session directory: %s", err)
			}

			options := "mode=0620,newinstance,ptmxmode=0666"
			if !c.userNS {
				group, err := user.GetGrNam("tty")
				if err != nil {
					return fmt.Errorf("Problem resolving 'tty' group GID: %s", err)
				}
				options = fmt.Sprintf("%s,gid=%d", options, group.GID)

			} else {
				sylog.Debugf("Not setting /dev/pts filesystem gid: user namespace enabled")
			}
			sylog.Debugf("Mounting devpts for staged /dev/pts")
			devptsPath, _ := c.session.GetPath("/dev/pts")
			err = system.Points.AddFS(mount.DevTag, devptsPath, "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, options)
			if err != nil {
				sylog.Verbosef("Couldn't mount devpts filesystem, continuing with PTY functionality disabled")
			} else {
				if err := c.bindDev("/dev/tty", system); err != nil {
					return err
				}
				if err := c.session.AddSymlink("/dev/ptmx", "/dev/pts/ptmx"); err != nil {
					return fmt.Errorf("failed to create /dev/ptmx symlink: %s", err)
				}
			}
		}
		if err := c.bindDev("/dev/null", system); err != nil {
			return err
		}
		if err := c.bindDev("/dev/zero", system); err != nil {
			return err
		}
		if err := c.bindDev("/dev/random", system); err != nil {
			return err
		}
		if err := c.bindDev("/dev/urandom", system); err != nil {
			return err
		}
		if c.engine.EngineConfig.GetNv() {
			files, err := ioutil.ReadDir("/dev")
			if err != nil {
				return fmt.Errorf("failed to read /dev directory: %s", err)
			}
			for _, file := range files {
				if strings.HasPrefix(file.Name(), "nvidia") {
					if err := c.bindDev(filepath.Join("/dev", file.Name()), system); err != nil {
						return err
					}
				}
			}
		}

		if err := c.session.AddSymlink("/dev/fd", "/proc/self/fd"); err != nil {
			return fmt.Errorf("failed to create symlink /dev/fd")
		}
		if err := c.session.AddSymlink("/dev/stdin", "/proc/self/fd/0"); err != nil {
			return fmt.Errorf("failed to create symlink /dev/stdin")
		}
		if err := c.session.AddSymlink("/dev/stdout", "/proc/self/fd/1"); err != nil {
			return fmt.Errorf("failed to create symlink /dev/stdout")
		}
		if err := c.session.AddSymlink("/dev/stderr", "/proc/self/fd/2"); err != nil {
			return fmt.Errorf("failed to create symlink /dev/stderr")
		}

		devPath, _ := c.session.GetPath("/dev")
		err = system.Points.AddBind(mount.DevTag, devPath, "/dev", syscall.MS_BIND|syscall.MS_NOSUID|syscall.MS_REC)
		if err != nil {
			return fmt.Errorf("unable to add dev to mount list: %s", err)
		}
	} else if c.engine.EngineConfig.File.MountDev == "yes" {
		sylog.Debugf("Adding dev to mount list\n")
		err := system.Points.AddBind(mount.DevTag, "/dev", "/dev", syscall.MS_BIND|syscall.MS_NOSUID|syscall.MS_REC)
		if err != nil {
			return fmt.Errorf("unable to add dev to mount list: %s", err)
		}
	} else if c.engine.EngineConfig.File.MountDev == "no" {
		sylog.Verbosef("Not mounting /dev inside the container")
	}
	return nil
}

func (c *container) addHostMount(system *mount.System) error {
	if !c.engine.EngineConfig.File.MountHostfs {
		sylog.Debugf("Not mounting host file systems per configuration")
		return nil
	}

	info, err := proc.ParseMountInfo("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)
	for _, child := range info["/"] {
		if strings.HasPrefix(child, "/proc") {
			sylog.Debugf("Skipping /proc based file system")
			continue
		} else if strings.HasPrefix(child, "/sys") {
			sylog.Debugf("Skipping /sys based file system")
			continue
		} else if strings.HasPrefix(child, "/dev") {
			sylog.Debugf("Skipping /dev based file system")
			continue
		} else if strings.HasPrefix(child, "/run") {
			sylog.Debugf("Skipping /run based file system")
			continue
		} else if strings.HasPrefix(child, "/boot") {
			sylog.Debugf("Skipping /boot based file system")
			continue
		} else if strings.HasPrefix(child, "/var") {
			sylog.Debugf("Skipping /var based file system")
			continue
		}
		sylog.Debugf("Adding %s to mount list\n", child)
		if err := system.Points.AddBind(mount.HostfsTag, child, child, flags); err != nil {
			return fmt.Errorf("unable to add %s to mount list: %s", child, err)
		}
		system.Points.AddRemount(mount.HostfsTag, child, flags)
	}
	return nil
}

func (c *container) addBindsMount(system *mount.System) error {
	flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)

	if c.engine.EngineConfig.GetContain() {
		sylog.Debugf("Skipping bind mounts as contain was requested")
		return nil
	}

	for _, bindpath := range c.engine.EngineConfig.File.BindPath {
		splitted := strings.Split(bindpath, ":")
		src := splitted[0]
		dst := ""
		if len(splitted) > 1 {
			dst = splitted[1]
		} else {
			dst = src
		}

		sylog.Verbosef("Found 'bind path' = %s, %s", src, dst)
		err := system.Points.AddBind(mount.BindsTag, src, dst, flags)
		if err != nil {
			return fmt.Errorf("unable to add %s to mount list: %s", src, err)
		}
	}

	return nil
}

func (c *container) addHomeMount(system *mount.System) error {
	if c.engine.EngineConfig.GetNoHome() {
		sylog.Debugf("Skipping home directory mount by user request.")
		return nil
	}
	if !c.engine.EngineConfig.File.MountHome {
		sylog.Debugf("Skipping home dir mounting (per config)")
		return nil
	}

	customHome := false

	pw, err := user.GetPwUID(uint32(os.Getuid()))
	if err != nil {
		return fmt.Errorf("failed to retrieve user information")
	}

	if pw.Dir != c.engine.EngineConfig.GetHome() {
		customHome = true
	}

	sylog.Debugf("Checking if user bind control is allowed")

	if customHome && !c.engine.EngineConfig.File.UserBindControl {
		return fmt.Errorf("Not mounting user requested home: user bind control is disallowed")
	}

	flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)

	sylog.Debugf("Adding home to mount list\n")
	homedir := strings.SplitN(c.engine.EngineConfig.GetHomeDir(), ":", 2)
	src, err := filepath.Abs(homedir[0])
	if err != nil {
		sylog.Warningf("Can't determine absolute path of %s home directory", homedir[0])
	}
	dst := src
	if len(homedir) == 2 {
		dst = homedir[1]
	}

	sylog.Debugf("Checking if overlay/underlay layers are enabled")

	if c.session.Layer == nil {
		sylog.Debugf("Staging home directory base")

		if err := c.session.AddDir(dst); err != nil {
			return fmt.Errorf("failed to add %s as session directory: %s", src, err)
		}
		sessionDir, _ := c.session.GetPath(dst)
		if err := system.Points.AddBind(mount.HomeTag, src, sessionDir, flags); err != nil {
			return fmt.Errorf("unable to add %s to mount list: %s", src, err)
		}
		system.Points.AddRemount(mount.HomeTag, sessionDir, flags)

		sylog.Debugf("Identifying the base home directory")

		dstDir := fs.RootDir(dst)
		if dstDir == "." {
			return fmt.Errorf("could not identify base home directory path: %s", dst)
		}
		sessionDir, _ = c.session.GetPath(dstDir)
		sylog.Verbosef("Mounting staged home directory base to container's base dir")
		if err := system.Points.AddBind(mount.FinalTag, sessionDir, dstDir, flags); err != nil {
			return fmt.Errorf("unable to add %s to mount list: %s", sessionDir, err)
		}
	} else {
		sylog.Debugf("Staging home directory")

		err := system.Points.AddBind(mount.HomeTag, src, dst, flags)
		if err != nil {
			return fmt.Errorf("unable to add home to mount list: %s", err)
		}
		system.Points.AddRemount(mount.HomeTag, dst, flags)
	}
	return nil
}

func (c *container) addUserbindsMount(system *mount.System) error {
	flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)

	if len(c.engine.EngineConfig.GetBindPath()) == 0 {
		return nil
	}

	sylog.Debugf("Checking for 'user bind control' in configuration file")
	if !c.engine.EngineConfig.File.UserBindControl {
		sylog.Warningf("Ignoring user bind request: user bind control disabled by system administrator")
		return nil
	}

	for _, b := range c.engine.EngineConfig.GetBindPath() {
		splitted := strings.Split(b, ":")

		src, err := filepath.Abs(splitted[0])
		if err != nil {
			sylog.Warningf("Can't determine absolute path of %s bind point", splitted[0])
			continue
		}
		dst := src
		if len(splitted) > 1 {
			dst = splitted[1]
		}
		if len(splitted) > 2 {
			if splitted[2] == "ro" {
				flags |= syscall.MS_RDONLY
			} else if splitted[2] != "rw" {
				sylog.Warningf("Not mounting requested %s bind point, invalid mount option %s", src, splitted[2])
			}
		}

		sylog.Debugf("Adding %s to mount list\n", src)
		if err := system.Points.AddBind(mount.UserbindsTag, src, dst, flags); err != nil {
			return fmt.Errorf("unabled to %s to mount list: %s", src, err)
		}
		system.Points.AddRemount(mount.UserbindsTag, dst, flags)
	}
	return nil
}

func (c *container) addTmpMount(system *mount.System) error {
	sylog.Debugf("Checking for 'mount tmp' in configuration file")
	if !c.engine.EngineConfig.File.MountTmp {
		sylog.Verbosef("Skipping tmp dir mounting (per config)")
		return nil
	}
	tmpSource := "/tmp"
	vartmpSource := "/var/tmp"

	if c.engine.EngineConfig.GetContain() {
		workdir := c.engine.EngineConfig.GetWorkdir()
		if workdir != "" {
			if !c.engine.EngineConfig.File.UserBindControl {
				sylog.Warningf("User bind control is disabled by system administrator")
				return nil
			}

			vartmpSource = "/var_tmp"

			workdir, err := filepath.Abs(filepath.Clean(workdir))
			if err != nil {
				sylog.Warningf("Can't determine absolute path of workdir %s", workdir)
			}

			tmpSource = workdir + tmpSource
			vartmpSource = workdir + vartmpSource

			if err := fs.MkdirAll(tmpSource, 0755); err != nil {
				return fmt.Errorf("failed to create %s: %s", tmpSource, err)
			}
			if err := fs.MkdirAll(vartmpSource, 0755); err != nil {
				return fmt.Errorf("failed to create %s: %s", vartmpSource, err)
			}
		} else {
			if err := c.session.AddDir(tmpSource); err != nil {
				return err
			}
			if err := c.session.AddDir(vartmpSource); err != nil {
				return err
			}
			tmpSource, _ = c.session.GetPath(tmpSource)
			vartmpSource, _ = c.session.GetPath(vartmpSource)
		}
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)

	if err := system.Points.AddBind(mount.TmpTag, tmpSource, "/tmp", flags); err == nil {
		system.Points.AddRemount(mount.TmpTag, "/tmp", flags)
	} else {
		return fmt.Errorf("could not mount container's /tmp directory: %s %s", err, tmpSource)
	}
	if err := system.Points.AddBind(mount.TmpTag, vartmpSource, "/var/tmp", flags); err == nil {
		system.Points.AddRemount(mount.TmpTag, "/var/tmp", flags)
	} else {
		return fmt.Errorf("could not mount container's /var/tmp directory: %s", err)
	}
	return nil
}

func (c *container) addScratchMount(system *mount.System) error {
	hasWorkdir := false

	scratchdir := c.engine.EngineConfig.GetScratchDir()
	if len(scratchdir) == 0 {
		sylog.Debugf("Not mounting scratch directory: Not requested")
		return nil
	} else if len(scratchdir) == 1 {
		scratchdir = strings.Split(filepath.Clean(scratchdir[0]), ",")
	}
	if !c.engine.EngineConfig.File.UserBindControl {
		sylog.Verbosef("Not mounting scratch: user bind control disabled by system administrator")
		return nil
	}
	workdir := c.engine.EngineConfig.GetWorkdir()
	sourceDir := ""
	if workdir != "" {
		hasWorkdir = true
		sourceDir = filepath.Clean(workdir) + "/scratch"
	} else {
		sourceDir = c.session.Path()
	}
	if hasWorkdir {
		if err := fs.MkdirAll(sourceDir, 0750); err != nil {
			return fmt.Errorf("could not create scratch working directory %s: %s", sourceDir, err)
		}
	}
	for _, dir := range scratchdir {
		fullSourceDir := ""

		if hasWorkdir {
			fullSourceDir = filepath.Join(sourceDir, filepath.Base(dir))
			if err := fs.MkdirAll(fullSourceDir, 0750); err != nil {
				return fmt.Errorf("could not create scratch working directory %s: %s", sourceDir, err)
			}
		} else {
			src := filepath.Join("/scratch", filepath.Base(dir))
			if err := c.session.AddDir(src); err != nil {
				return fmt.Errorf("could not create scratch working directory %s: %s", sourceDir, err)
			}
			fullSourceDir, _ = c.session.GetPath(src)
		}
		flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)
		if err := system.Points.AddBind(mount.ScratchTag, fullSourceDir, dir, flags); err != nil {
			return fmt.Errorf("could not bind scratch directory %s into container: %s", fullSourceDir, err)
		}
		system.Points.AddRemount(mount.ScratchTag, dir, flags)
	}
	return nil
}

func (c *container) addCwdMount(system *mount.System) error {
	cwd := ""

	if c.engine.EngineConfig.GetContain() {
		sylog.Verbosef("Not mounting current directory: container was requested")
		return nil
	}
	if !c.engine.EngineConfig.File.UserBindControl {
		sylog.Warningf("Not mounting current directory: user bind control is disabled by system administrator")
		return nil
	}
	if c.engine.CommonConfig.OciConfig.Process == nil {
		return nil
	}
	cwd = c.engine.CommonConfig.OciConfig.Process.Cwd
	if err := os.Chdir(cwd); err != nil {
		sylog.Debugf("can't go to container working directory: %s", err)
		return nil
	}
	current, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("could not obtain current directory path: %s", err)
	}
	switch current {
	case "/", "/etc", "/bin", "/mnt", "/usr", "/var", "/opt", "/sbin":
		sylog.Verbosef("Not mounting CWD within operating system directory: %s", current)
		return nil
	}
	if strings.HasPrefix(current, "/sys") || strings.HasPrefix(current, "/proc") || strings.HasPrefix(current, "/dev") {
		sylog.Verbosef("Not mounting CWD within virtual directory: %s", current)
		return nil
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)
	if err := system.Points.AddBind(mount.CwdTag, current, cwd, flags); err == nil {
		system.Points.AddRemount(mount.CwdTag, cwd, flags)
	} else {
		sylog.Warningf("Could not bind CWD to container %s: %s", current, err)
	}
	return nil
}

func (c *container) addLibsMount(system *mount.System) error {
	return nil
}

func (c *container) addFilesMount(system *mount.System) error {
	if os.Geteuid() == 0 {
		sylog.Verbosef("Not updating passwd/group files, running as root!")
		return nil
	}

	rootfs := c.session.RootFsPath()
	defer c.session.Update()

	if c.engine.EngineConfig.File.ConfigPasswd {
		passwd := filepath.Join(rootfs, "/etc/passwd")
		content, err := files.Passwd(passwd, c.engine.EngineConfig.GetHome())
		if err != nil {
			sylog.Warningf("%s", err)
		} else {
			if err := c.session.AddFile("/etc/passwd", content); err != nil {
				sylog.Warningf("failed to add passwd session file: %s", err)
			}
			passwd, _ = c.session.GetPath("/etc/passwd")

			sylog.Debugf("Adding /etc/passwd to mount list\n")
			err = system.Points.AddBind(mount.FilesTag, passwd, "/etc/passwd", syscall.MS_BIND)
			if err != nil {
				return fmt.Errorf("unable to add /etc/passwd to mount list: %s", err)
			}
		}
	} else {
		sylog.Verbosef("Skipping bind of the host's /etc/passwd")
	}

	if c.engine.EngineConfig.File.ConfigGroup {
		group := filepath.Join(rootfs, "/etc/group")
		content, err := files.Group(group)
		if err != nil {
			sylog.Warningf("%s", err)
		} else {
			if err := c.session.AddFile("/etc/group", content); err != nil {
				sylog.Warningf("failed to add group session file: %s", err)
			}
			group, _ = c.session.GetPath("/etc/group")

			sylog.Debugf("Adding /etc/group to mount list\n")
			err = system.Points.AddBind(mount.FilesTag, group, "/etc/group", syscall.MS_BIND)
			if err != nil {
				return fmt.Errorf("unable to add /etc/group to mount list: %s", err)
			}
		}
	} else {
		sylog.Verbosef("Skipping bind of the host's /etc/group")
	}

	return nil
}
