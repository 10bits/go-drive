package drive

import (
	"go-drive/common"
	"go-drive/common/drive_util"
	"go-drive/common/task"
	"go-drive/common/types"
	"go-drive/storage"
	"io"
	"log"
	fsPath "path"
	"regexp"
	"strings"
	"sync"
)

var pathRegexp = regexp.MustCompile(`^/?([^/]+)(/(.*))?$`)

// DispatcherDrive splits drive name and key from the raw key.
// Then dispatch request to the specified drive.
type DispatcherDrive struct {
	drives map[string]types.IDrive
	mounts map[string]map[string]types.PathMount

	mountStorage *storage.PathMountStorage
	mux          *sync.Mutex
}

func NewDispatcherDrive(mountStorage *storage.PathMountStorage) *DispatcherDrive {
	return &DispatcherDrive{
		drives:       make(map[string]types.IDrive),
		mountStorage: mountStorage,
		mux:          &sync.Mutex{},
	}
}

func (d *DispatcherDrive) setDrives(drives map[string]types.IDrive) {
	d.mux.Lock()
	defer d.mux.Unlock()
	for _, d := range d.drives {
		if disposable, ok := d.(types.IDisposable); ok {
			_ = disposable.Dispose()
		}
	}
	newDrives := make(map[string]types.IDrive, len(drives))
	for k, v := range drives {
		newDrives[k] = v
	}
	d.drives = newDrives
}

func (d *DispatcherDrive) reloadMounts() error {
	d.mux.Lock()
	defer d.mux.Unlock()
	mounts, e := d.mountStorage.GetMounts()
	if e != nil {
		return e
	}
	ms := make(map[string]map[string]types.PathMount, 0)
	for _, m := range mounts {
		t := ms[*m.Path]
		if t == nil {
			t = make(map[string]types.PathMount, 0)
		}
		t[m.Name] = m
		ms[*m.Path] = t
	}

	d.mounts = ms
	return nil
}

func (d *DispatcherDrive) Meta() types.DriveMeta {
	log.Fatalln("not supported")
	return types.DriveMeta{}
}

func (d *DispatcherDrive) resolve(path string) (types.IDrive, string, error) {
	targetPath := d.resolveMount(path)
	if targetPath != "" {
		path = targetPath
	}
	paths := pathRegexp.FindStringSubmatch(path)
	if paths == nil {
		return nil, "", common.NewNotFoundError()
	}
	driveName := paths[1]
	entryPath := paths[3]
	drive, ok := d.drives[driveName]
	if !ok {
		return nil, "", common.NewNotFoundError()
	}
	return drive, entryPath, nil
}

func (d *DispatcherDrive) resolveMount(path string) string {
	tree := common.PathParentTree(path)
	var mountAt, prefix string
	for _, p := range tree {
		dir := common.PathParent(p)
		name := common.PathBase(p)
		temp := d.mounts[dir]
		if temp != nil {
			mountAt = temp[name].MountAt
			if mountAt != "" {
				prefix = p
				break
			}
		}
	}
	if mountAt == "" {
		return ""
	}

	return fsPath.Join(
		mountAt,
		common.CleanPath(path[len(prefix):]),
	)
}

func (d *DispatcherDrive) resolveMountedChildren(path string) ([]types.PathMount, bool) {
	result := make([]types.PathMount, 0)
	isSelf := false
	for mountParent, mounts := range d.mounts {
		for mountName, m := range mounts {
			if strings.HasPrefix(fsPath.Join(mountParent, mountName), path) {
				result = append(result, m)
				if !isSelf && fsPath.Join(*m.Path, m.Name) == path {
					isSelf = true
				}
			}
		}
	}
	return result, isSelf
}

func (d *DispatcherDrive) Get(path string) (types.IEntry, error) {
	if common.IsRootPath(path) {
		return &driveEntry{d: d, path: "", name: "", meta: types.DriveMeta{
			CanWrite: false,
		}}, nil
	}
	drive, realPath, e := d.resolve(path)
	if e != nil {
		return nil, e
	}
	entry, e := drive.Get(realPath)
	if e != nil {
		return nil, e
	}
	return d.mapDriveEntry(path, entry), nil
}

func (d *DispatcherDrive) Save(path string, size int64, override bool, reader io.Reader, ctx types.TaskCtx) (types.IEntry, error) {
	drive, realPath, e := d.resolve(path)
	if e != nil {
		return nil, e
	}
	save, e := drive.Save(realPath, size, override, reader, ctx)
	if e != nil {
		return nil, e
	}
	return d.mapDriveEntry(path, save), nil
}

func (d *DispatcherDrive) MakeDir(path string) (types.IEntry, error) {
	drive, realPath, e := d.resolve(path)
	if e != nil {
		return nil, e
	}
	dir, e := drive.MakeDir(realPath)
	if e != nil {
		return nil, e
	}
	return d.mapDriveEntry(path, dir), nil
}

func (d *DispatcherDrive) Copy(from types.IEntry, to string, override bool, ctx types.TaskCtx) (types.IEntry, error) {
	driveTo, pathTo, e := d.resolve(to)
	if e != nil {
		return nil, e
	}
	mounts, _ := d.resolveMountedChildren(from.Path())
	if len(mounts) == 0 {
		// if `from` has no mounted children, then copy
		entry, e := driveTo.Copy(from, pathTo, override, ctx)
		if e == nil {
			return entry, nil
		}
		if !common.IsUnsupportedError(e) {
			return nil, e
		}
	}
	// if `from` has mounted children, we need to copy them
	e = drive_util.CopyAll(from, d, to, override, ctx,
		func(from types.IEntry, _ types.IDrive, to string, ctx types.TaskCtx) error {
			driveTo, pathTo, e := d.resolve(to)
			ctxWrapper := task.NewCtxWrapper(ctx, true, false)
			if e != nil {
				return e
			}
			_, e = driveTo.Copy(from, pathTo, true, ctxWrapper)
			if e == nil {
				return nil
			}
			if !common.IsUnsupportedError(e) {
				return e
			}
			return drive_util.CopyEntry(from, driveTo, pathTo, true, ctxWrapper)
		}, nil)
	if e != nil {
		return nil, e
	}
	copied, e := driveTo.Get(pathTo)
	if e != nil {
		return nil, e
	}
	return d.mapDriveEntry(to, copied), nil
}

func (d *DispatcherDrive) Move(from types.IEntry, to string, override bool, ctx types.TaskCtx) (types.IEntry, error) {
	driveTo, pathTo, e := d.resolve(to)
	// if path depth is 1, move mounts
	if e != nil && common.PathDepth(to) != 1 {
		return nil, e
	}
	fromPath := from.Path()
	children, isSelf := d.resolveMountedChildren(fromPath)
	if len(children) > 0 {
		movedMounts := make([]types.PathMount, 0, len(children))
		for _, m := range children {
			t := fsPath.Join(
				to,
				fsPath.Join(*m.Path, m.Name)[len(fromPath):],
			)
			mPath := common.PathParent(t)
			m.Path = &mPath
			m.Name = common.PathBase(t)
			movedMounts = append(movedMounts, m)
		}
		if e := d.mountStorage.DeleteAndSaveMounts(children, movedMounts, true); e != nil {
			return nil, e
		}
		_ = d.reloadMounts()
		if isSelf {
			return d.Get(to)
		}
	} else {
		// no mounts matched and toPath is in root
		if driveTo == nil {
			return nil, common.NewNotAllowedError()
		}
	}
	if driveTo != nil {
		move, e := driveTo.Move(from, pathTo, override, ctx)
		if e != nil {
			if common.IsUnsupportedError(e) {
				return nil, common.NewNotAllowedMessageError("move across drives is not supported")
			}
			return nil, e
		}
		return d.mapDriveEntry(to, move), nil
	}
	return d.Get(to)
}

func (d *DispatcherDrive) List(path string) ([]types.IEntry, error) {
	var entries []types.IEntry
	if common.IsRootPath(path) {
		drives := make([]types.IEntry, 0, len(d.drives))
		for k, v := range d.drives {
			drives = append(drives, &driveEntry{d: d, path: k, name: k, meta: v.Meta()})
		}
		entries = drives
	} else {
		drive, realPath, e := d.resolve(path)
		if e != nil {
			return nil, e
		}
		list, e := drive.List(realPath)
		if e != nil {
			return nil, e
		}
		entries = d.mapDriveEntries(path, list)
	}

	ms := d.mounts[path]
	if ms != nil {
		mountedMap := make(map[string]types.IEntry, len(entries))
		for name, m := range ms {
			drive, entryPath, e := d.resolve(m.MountAt)
			if e != nil {
				continue
			}
			entry, e := drive.Get(entryPath)
			if e != nil {
				if common.IsNotFoundError(e) {
					continue
				}
				return nil, e
			}
			mountedMap[name] = &entryWrapper{d: d, path: fsPath.Join(path, name), entry: entry, isMount: true}
		}

		newEntries := make([]types.IEntry, 0, len(entries)+len(mountedMap))
		for _, e := range entries {
			if mountedMap[common.PathBase(e.Path())] == nil {
				newEntries = append(newEntries, e)
			}
		}
		for _, e := range mountedMap {
			newEntries = append(newEntries, e)
		}
		entries = newEntries
	}
	return entries, nil
}

func (d *DispatcherDrive) Delete(path string, ctx types.TaskCtx) error {
	children, isSelf := d.resolveMountedChildren(path)
	if len(children) > 0 {
		e := d.mountStorage.DeleteMounts(children)
		if e != nil {
			return e
		}
		_ = d.reloadMounts()
		if isSelf {
			return nil
		}
	}
	drive, path, e := d.resolve(path)
	if e != nil {
		return e
	}
	if common.IsRootPath(path) {
		return common.NewNotAllowedError()
	}
	return drive.Delete(path, ctx)
}

func (d *DispatcherDrive) Upload(path string, size int64,
	override bool, config types.SM) (*types.DriveUploadConfig, error) {
	drive, path, e := d.resolve(path)
	if e != nil {
		return nil, e
	}
	return drive.Upload(path, size, override, config)
}

func (d *DispatcherDrive) mapDriveEntry(path string, entry types.IEntry) types.IEntry {
	return &entryWrapper{d: d, path: path, entry: entry}
}

func (d *DispatcherDrive) mapDriveEntries(dir string, entries []types.IEntry) []types.IEntry {
	mappedEntries := make([]types.IEntry, 0, len(entries))
	for _, e := range entries {
		path := e.Path()
		mappedEntries = append(
			mappedEntries,
			d.mapDriveEntry(fsPath.Join(dir, common.PathBase(path)), e),
		)
	}
	return mappedEntries
}

type entryWrapper struct {
	d       *DispatcherDrive
	path    string
	entry   types.IEntry
	isMount bool
}

func (d *entryWrapper) Path() string {
	return d.path
}

func (d *entryWrapper) Type() types.EntryType {
	return d.entry.Type()
}

func (d *entryWrapper) Size() int64 {
	return d.entry.Size()
}

func (d *entryWrapper) Meta() types.EntryMeta {
	meta := d.entry.Meta()
	if d.isMount {
		meta.Props = common.CopyMap(meta.Props)
		meta.Props["is_mount"] = true
	}
	return meta
}

func (d *entryWrapper) ModTime() int64 {
	return d.entry.ModTime()
}

func (d *entryWrapper) Name() string {
	return common.PathBase(d.path)
}

func (d *entryWrapper) GetReader() (io.ReadCloser, error) {
	if content, ok := d.entry.(types.IContent); ok {
		return content.GetReader()
	}
	return nil, common.NewNotAllowedError()
}

func (d *entryWrapper) GetURL() (string, bool, error) {
	if content, ok := d.entry.(types.IContent); ok {
		return content.GetURL()
	}
	return "", false, common.NewNotAllowedError()
}

func (d *entryWrapper) Drive() types.IDrive {
	return d.d
}

func (d *entryWrapper) GetIEntry() types.IEntry {
	return d.entry
}

type driveEntry struct {
	d    *DispatcherDrive
	path string
	name string
	meta types.DriveMeta
}

func (d *driveEntry) Path() string {
	return d.path
}

func (d *driveEntry) Type() types.EntryType {
	return types.TypeDir
}

func (d *driveEntry) Size() int64 {
	return -1
}

func (d *driveEntry) Meta() types.EntryMeta {
	return types.EntryMeta{CanRead: true, CanWrite: false, Props: d.meta.Props}
}

func (d *driveEntry) ModTime() int64 {
	return -1
}

func (d *driveEntry) Name() string {
	return d.name
}

func (d *driveEntry) GetReader() (io.ReadCloser, error) {
	return nil, common.NewNotAllowedError()
}

func (d *driveEntry) GetURL() (string, bool, error) {
	return "", false, common.NewNotAllowedError()
}

func (d *driveEntry) Drive() types.IDrive {
	return d.d
}
