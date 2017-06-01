// Package local implemenst the filesystem interfaces on top of the
// operating system's filesystem.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"zenhack.net/go/sandstorm-filesystem/filesystem"
	grain_capnp "zenhack.net/go/sandstorm/capnp/grain"
	util_capnp "zenhack.net/go/sandstorm/capnp/util"
	"zenhack.net/go/sandstorm/util"
	"zombiezen.com/go/capnproto2"
)

var (
	InvalidArgument = errors.New("Invalid argument")
	IllegalFileName = errors.New("Illegal file name")
	OpenFailed      = errors.New("Open failed")
	NotImplemented  = errors.New("Not implemented")
)

func NewNode(path string) (*Node, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &Node{
		path:       path,
		isDir:      fi.IsDir(),
		writable:   fi.Mode()&0200 != 0,
		executable: fi.Mode()&0100 != 0,
	}, nil
}

type Node struct {
	isDir      bool
	writable   bool
	executable bool
	path       string
}

func (n *Node) Save(p grain_capnp.AppPersistent_save) error {
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	u8list, err := capnp.NewData(p.Results.Struct.Segment(), data)
	if err != nil {
		return err
	}
	p.Results.SetObjectIdPtr(u8list.List.ToPtr())
	return nil
}

func (n *Node) Restore(p grain_capnp.MainView_restore) error {
	ptr, err := p.Params.ObjectIdPtr()
	if err != nil {
		return err
	}
	err = json.Unmarshal(ptr.Data(), n)
	if err != nil {
		return err
	}
	capId := p.Results.Struct.Segment().Message().AddCap(n.MakeClient().Client)
	p.Results.SetCapPtr(capnp.NewInterface(p.Results.Struct.Segment(), capId).ToPtr())
	return nil
}

func (n *Node) Stat(p filesystem.Node_stat) error {
	fi, err := os.Stat(n.path)
	if err != nil {
		// TODO: think about the right way to handle this.
		return err
	}
	info, err := p.Results.Info()
	if err != nil {
		return err
	}
	if n.isDir {
		info.SetDir()
	} else {
		info.SetFile()
		info.File().SetSize(fi.Size())
	}
	info.SetWritable(n.writable)
	info.SetExecutable(n.executable)
	return nil
}

type cancelHandle context.CancelFunc

func (c cancelHandle) Close() error {
	c()
	return nil
}

func (d *Node) List(p filesystem.Directory_list) error {
	stream := p.Params.Stream()
	file, err := os.Open(d.path)
	if err != nil {
		// err might contain private info, e.g. where the directory
		// is rooted. So we return a generic error. It might be nice
		// to find some way to allow more information for debugging.
		return OpenFailed
	}
	ctx, cancel := context.WithCancel(p.Ctx)
	p.Results.SetCancel(util_capnp.Handle_ServerToClient(cancelHandle(cancel)))
	go func() {
		defer file.Close()
		defer stream.Done(ctx, func(filesystem.Directory_Entry_Stream_done_Params) error {
			return nil
		})

		maxBufSize := 1024

		for ctx.Err() == nil {
			fis, err := file.Readdir(maxBufSize)
			if err != nil {
				// TODO: can we communicate failures somehow? This
				// could mean EOF or a legitmate problem, but we don't
				// currently have a good way to convey the latter to the
				// caller
				return
			}

			stream.Push(ctx, func(p filesystem.Directory_Entry_Stream_push_Params) error {
				list, err := p.NewEntries(int32(len(fis)))
				if err != nil {
					return err
				}
				for i := range fis {
					fi := fis[i]
					ent := list.At(i)
					ent.SetName(fi.Name())
					info, err := ent.Info()
					if err != nil {
						// TODO FIXME: error reporting.
						return err
					}
					info.SetWritable(d.writable && (fi.Mode()&0200 != 0))
					info.SetExecutable(fi.Mode()&0100 != 0)
					if fi.IsDir() {
						info.SetDir()
					} else {
						info.SetFile()
						info.File().SetSize(fi.Size())
					}
				}
				return nil
			})
		}

	}()
	return nil
}

func (d *Node) Walk(p filesystem.Directory_walk) error {
	name, err := p.Params.Name()
	if err != nil {
		return err
	}

	if !validFileName(name) {
		return IllegalFileName
	}

	path := d.path + "/" + name
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}

	node := &Node{
		path:       path,
		isDir:      fi.IsDir(),
		writable:   d.writable && fi.Mode()&0200 != 0,
		executable: fi.Mode()&0100 != 0,
	}

	p.Results.SetNode(node.MakeClient())
	return nil
}

func (d *Node) Create(p filesystem.RwDirectory_create) error {
	name, err := p.Params.Name()
	if err != nil {
		return err
	}
	if !validFileName(name) {
		return IllegalFileName
	}

	node := Node{
		path:       d.path + "/" + name,
		executable: p.Params.Executable(),
		writable:   true,
	}

	mode := os.FileMode(0644)
	if node.executable {
		mode |= 0111
	}

	file, err := os.OpenFile(node.path, os.O_RDWR|os.O_CREATE, mode)
	if err != nil {
		return OpenFailed
	}
	file.Close()

	p.Results.SetFile(filesystem.RwFile_ServerToClient(&node))
	return nil
}

func (d *Node) Mkdir(p filesystem.RwDirectory_mkdir) error {
	return NotImplemented
}

func (d *Node) Delete(p filesystem.RwDirectory_delete) error {
	return NotImplemented
}

func validFileName(name string) bool {
	return name != "" &&
		name != "." &&
		name != ".." &&
		!strings.Contains(name, "/")
}

func (n *Node) MakeClient() filesystem.Node {
	var client capnp.Client
	if n.isDir {
		if n.writable {
			client = filesystem.RwDirectory_ServerToClient(n).Client
		} else {
			client = filesystem.Directory_ServerToClient(n).Client
		}
	} else {
		if n.writable {
			client = filesystem.RwFile_ServerToClient(n).Client
		} else {
			client = filesystem.File_ServerToClient(n).Client
		}
	}
	return filesystem.Node{Client: client}
}

func (f *Node) Write(p filesystem.RwFile_write) error {
	startAt := p.Params.StartAt()

	if startAt <= -2 {
		return InvalidArgument
	}

	file, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if startAt == -1 {
		_, err = file.Seek(0, 2)
	} else {
		_, err = file.Seek(startAt, 0)
	}
	if err != nil {
		return err
	}
	bs := util_capnp.ByteStream_ServerToClient(&util.WriteCloserByteStream{
		WC: file,
	})
	p.Results.SetSink(bs)
	return nil
}

func (f *Node) SetExec(p filesystem.RwFile_setExec) error {
	exec := p.Params.Exec()
	fi, err := os.Stat(f.path)
	// FIXME: censor error like with OpenFailed.
	if err != nil {
		return err
	}
	if exec {
		// FIXME: censor error like with OpenFailed.
		return os.Chmod(f.path, fi.Mode()|0111)
	} else {
		// FIXME: censor error like with OpenFailed.
		return os.Chmod(f.path, fi.Mode()&^0111)
	}
}

func (f *Node) Truncate(p filesystem.RwFile_truncate) error {
	// FIXME: cast/overflow issues.
	if err := os.Truncate(f.path, int64(p.Params.Size())); err != nil {
		return OpenFailed
	}
	return nil
}

func (f *Node) Read(p filesystem.File_read) error {
	startAt := p.Params.StartAt()
	if startAt < 0 {
		return InvalidArgument
	}

	amount := int64(p.Params.Amount())
	if amount < 0 {
		// The go api expects a signed value, so if we get something
		// greater than an int64 can represent, we just say "read the
		// whole thing." That's a stupid amount of data, so it's always
		// going to do the same thing anyway.
		amount = 0
	}
	sink := p.Params.Sink()

	file, err := os.Open(f.path)
	if err != nil {
		return OpenFailed
	}

	ctx, cancel := context.WithCancel(p.Ctx)
	p.Results.SetCancel(util_capnp.Handle_ServerToClient(cancelHandle(cancel)))

	go func() {
		defer file.Close()
		wc := util.ByteStreamWriteCloser{ctx, sink}
		defer wc.Close()
		_, err := file.Seek(startAt, 0)
		r := io.Reader(file)
		if amount != 0 {
			r = io.LimitReader(r, amount)
		}
		if err != nil {
			return
		}
		io.Copy(wc, r)
	}()
	return nil
}