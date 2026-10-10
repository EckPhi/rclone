// Package idrive implements experimental IDrive Cloud Drive access.
package idrive

import (
	"context"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/idrive/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/readers"
)

const defaultEncoding = encoder.EncodeSlash | encoder.EncodeWin | encoder.EncodeBackSlash | encoder.EncodeHashPercent | encoder.EncodeSingleQuote | encoder.EncodeBackQuote | encoder.EncodeDollar | encoder.EncodeCtl | encoder.EncodeDel | encoder.EncodeLeftSpace | encoder.EncodeRightSpace | encoder.EncodeLeftPeriod | encoder.EncodeRightPeriod | encoder.EncodeLeftTilde | encoder.EncodeInvalidUtf8 | encoder.EncodeDot | encoder.EncodeSquareBracket | encoder.EncodeSemicolon | encoder.EncodeExclamation

func init() {
	fs.Register(&fs.RegInfo{
		Name: "idrive", Description: "IDrive Cloud Drive (experimental)", NewFs: NewFs, Config: configure,
		Options: []fs.Option{
			{Name: config.ConfigEncoding, Help: config.ConfigEncodingHelp, Default: defaultEncoding, Advanced: true},
			{Name: "endpoint", Hide: fs.OptionHideConfigurator, Help: "Cloud Drive HTTPS origin from your web session, for example https://evssyncweb63.idrive.com."},
			{Name: "token", Hide: fs.OptionHideConfigurator, IsPassword: true, Sensitive: true, Help: "Cloud Drive token for manual access or the browser session cache."},
			{Name: "access_token", Hide: fs.OptionHideConfigurator, IsPassword: true, Sensitive: true, Help: "Browser web access token used to renew Cloud Drive authorization."},
		},
	})
}

func saveSession(m configmap.Mapper) func(string, string, string) {
	return func(access, drive, endpoint string) {
		m.Set("access_token", obscure.MustObscure(access))
		if drive != "" {
			m.Set("token", obscure.MustObscure(drive))
		}
		m.Set("endpoint", endpoint)
	}
}

func configure(ctx context.Context, name string, m configmap.Mapper, in fs.ConfigIn) (*fs.ConfigOut, error) {
	switch in.State {
	case "", "choose_auth":
		return fs.ConfigChooseExclusiveFixed("auth_mode", "config_auth_mode", "Authorization method", []fs.OptionExample{{Value: "browser", Help: "Sign in with your browser and import session authorization"}, {Value: "manual", Help: "Enter a Cloud Drive endpoint and token manually"}})
	case "auth_mode":
		if in.Result == "manual" {
			return fs.ConfigInput("manual_endpoint", "config_endpoint", "Cloud Drive HTTPS server origin.")
		}
		if in.Result != "browser" {
			return nil, errors.New("idrive: invalid authorization method")
		}
		return fs.ConfigConfirm("browser_open", true, "config_browser_local", "Open the browser on this machine? Choose No when configuring over SSH or on a server.")
	case "browser_open":
		if in.Result == "true" {
			_ = oauthutil.OpenURL("https://www.idrive.com/app/webconsole/drive/all/")
		}
		return fs.ConfigPassword("browser_token", "config_access_token", `On a machine with a browser, open:
https://www.idrive.com/app/webconsole/drive/all/
Sign in and unlock Cloud Drive if it uses a private key.
In DevTools -> Console run:
console.log(localStorage.getItem("Token"))
Copy the printed token without quotes and paste it here, including when this
rclone prompt is running over SSH. It will not be displayed.
If the result is null, finish signing in and open Cloud Drive, then try again.`)
	case "browser_token":
		access, err := obscure.Reveal(in.Result)
		if err != nil {
			return fs.ConfigError("choose_auth", "Could not read the browser access token.")
		}
		return finishBrowserConfig(ctx, m, access)

	case "manual_endpoint":
		m.Set("endpoint", in.Result)
		return fs.ConfigPassword("manual_token", "config_token", "Cloud Drive token.")
	case "manual_token":
		token, err := obscure.Reveal(in.Result)
		if err != nil {
			return fs.ConfigError("choose_auth", "Could not read the Cloud Drive token.")
		}
		endpoint, _ := m.Get("endpoint")
		if _, err = api.NewClient(endpoint, token, nil); err != nil {
			return fs.ConfigError("choose_auth", err.Error())
		}
		m.Set("access_token", "")
		m.Set("token", in.Result)
		return nil, nil
	}
	return nil, errors.New("idrive: unknown configuration state")
}

func finishBrowserConfig(ctx context.Context, m configmap.Mapper, access string) (*fs.ConfigOut, error) {
	var savedAccess, savedDrive, savedEndpoint string
	session, err := api.NewSession(access, nil, func(a, d, e string) { savedAccess, savedDrive, savedEndpoint = a, d, e })
	if err != nil {
		return fs.ConfigError("choose_auth", err.Error())
	}
	endpoint, token, err := session.Credentials(ctx, "")
	if err != nil {
		return fs.ConfigError("choose_auth", err.Error())
	}
	client, err := api.NewClient(endpoint, token, nil)
	if err != nil {
		return nil, err
	}
	client.UseSession(session)
	if _, err = client.List(ctx, "/"); err != nil {
		return nil, err
	}
	saveSession(m)(savedAccess, savedDrive, savedEndpoint)
	return nil, nil
}

// Options defines browser authorization and manual Cloud Drive access.
type Options struct {
	Enc         encoder.MultiEncoder `config:"encoding"`
	Endpoint    string               `config:"endpoint"`
	Token       string               `config:"token"`
	AccessToken string               `config:"access_token"`
}

// Fs stores a Cloud Drive filesystem rooted at a remote directory.
type Fs struct {
	name, root string
	client     *api.Client
	features   *fs.Features
	enc        encoder.MultiEncoder
}

// Object describes a Cloud Drive file.
type Object struct {
	f       *Fs
	remote  string
	size    int64
	modTime time.Time
}

// NewFs constructs a filesystem using browser authorization or a manual token.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := Options{Enc: defaultEncoding}
	if err := configstruct.Set(m, &opt); err != nil {
		return nil, err
	}
	var client *api.Client
	var err error
	if opt.AccessToken != "" {
		access, e := obscure.Reveal(opt.AccessToken)
		if e != nil {
			return nil, errors.New("idrive: browser access token must be obscured")
		}
		session, e := api.NewSession(access, nil, saveSession(m))
		if e != nil {
			return nil, e
		}
		endpoint, token, e := session.Credentials(ctx, "")
		if e != nil {
			return nil, e
		}
		client, err = api.NewClient(endpoint, token, nil)
		if err == nil {
			client.UseSession(session)
		}
	} else {
		token, e := obscure.Reveal(opt.Token)
		if e != nil {
			return nil, errors.New("idrive: token must be obscured with rclone obscure")
		}
		client, err = api.NewClient(opt.Endpoint, token, nil)
	}
	if err != nil {
		return nil, err
	}
	root = strings.Trim(root, "/")
	if err := validPath(root); err != nil {
		return nil, err
	}
	f := &Fs{name: name, root: root, client: client, enc: opt.Enc}
	f.features = (&fs.Features{CanHaveEmptyDirectories: true}).Fill(ctx, f)
	if root != "" {
		parent, base := path.Split(root)
		listing, err := client.List(ctx, encodePath(opt.Enc, "/"+strings.TrimRight(parent, "/")))
		if errors.Is(err, api.ErrNotFound) {
			return f, nil
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range listing.Contents {
			if entry.Name == opt.Enc.FromStandardName(base) && !entry.IsDir {
				f.root = strings.TrimRight(parent, "/")
				return f, fs.ErrorIsFile
			}
		}
	}
	return f, nil
}

// IDrive rejects periods inside directory names. Escape them after the
// standard encoder, and quote existing fullwidth periods to preserve names.
func encodePath(enc encoder.MultiEncoder, remote string) string {
	return strings.ReplaceAll(strings.ReplaceAll(enc.FromStandardPath(remote), "．", "．．"), ".", "．·")
}
func decodePeriods(name string) string {
	var out strings.Builder
	for len(name) > 0 {
		if strings.HasPrefix(name, "．．") {
			out.WriteString("．")
			name = strings.TrimPrefix(name, "．．")
		} else if strings.HasPrefix(name, "．·") {
			out.WriteByte('.')
			name = strings.TrimPrefix(name, "．·")
		} else {
			out.WriteByte(name[0])
			name = name[1:]
		}
	}
	return out.String()
}

func validPath(remote string) error {
	for _, part := range strings.Split(remote, "/") {
		if part == ".." || part == "." || strings.ContainsRune(part, '\x00') {
			return errors.New("idrive: invalid remote path")
		}
	}
	return nil
}

func (f *Fs) absolute(remote string) (string, error) {
	if err := validPath(remote); err != nil {
		return "", err
	}
	return encodePath(f.enc, path.Join("/", f.root, remote)), nil
}

func (f *Fs) fileAbsolute(remote string) (string, error) {
	if err := validPath(remote); err != nil {
		return "", err
	}
	parent, base := path.Split(remote)
	dir, err := f.absolute(strings.TrimRight(parent, "/"))
	if err != nil {
		return "", err
	}
	return path.Join(dir, f.enc.FromStandardName(base)), nil
}

// Name returns the configured remote name.
func (f *Fs) Name() string { return f.name }

// Root returns the configured remote root.
func (f *Fs) Root() string { return f.root }

// String describes the filesystem without exposing credentials.
func (f *Fs) String() string { return "IDrive Cloud Drive root '" + f.root + "'" }

// Precision reports that source modification times cannot be preserved reliably.
func (f *Fs) Precision() time.Duration { return fs.ModTimeNotSupported }

// Hashes returns no hashes because the observed checksum is unavailable.
func (f *Fs) Hashes() hash.Set { return hash.NewHashSet() }

// Features returns the supported optional operations.
func (f *Fs) Features() *fs.Features { return f.features }

func entryTime(entry api.Entry) time.Time {
	if epoch, err := strconv.ParseInt(entry.ModTimeUnix, 10, 64); err == nil {
		return time.Unix(epoch, 0)
	}
	if t, err := time.Parse("2006/01/02 15:04:05", entry.ModTime); err == nil {
		return t
	}
	return time.Unix(0, 0)
}

// List returns the files and directories directly within dir.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	abs, err := f.absolute(dir)
	if err != nil {
		return nil, err
	}
	listing, err := f.client.List(ctx, abs)
	if errors.Is(err, api.ErrNotFound) {
		return nil, fs.ErrorDirNotFound
	}
	if err != nil {
		return nil, err
	}
	entries := make(fs.DirEntries, 0, len(listing.Contents))
	for _, entry := range listing.Contents {
		if entry.IsDir {
			entry.Name = decodePeriods(entry.Name)
		}
		entry.Name = f.enc.ToStandardName(entry.Name)
		if entry.Name == "" || strings.Contains(entry.Name, "/") || validPath(entry.Name) != nil {
			return nil, errors.New("idrive: invalid entry name in listing")
		}
		remote := path.Join(dir, entry.Name)
		if entry.IsDir {
			entries = append(entries, fs.NewDir(remote, entryTime(entry)))
			continue
		}
		size, err := strconv.ParseInt(entry.Size, 10, 64)
		if err != nil || size < 0 {
			return nil, errors.New("idrive: invalid file size in listing")
		}
		entries = append(entries, &Object{f: f, remote: remote, size: size, modTime: entryTime(entry)})
	}
	return entries, nil
}

// NewObject resolves a file by listing its parent directory.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if err := validPath(remote); err != nil {
		return nil, err
	}
	parent := path.Dir(remote)
	if parent == "." {
		parent = ""
	}
	entries, err := f.List(ctx, parent)
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil, fs.ErrorObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Remote() == remote {
			if obj, ok := entry.(*Object); ok {
				return obj, nil
			}
			return nil, fs.ErrorIsDir
		}
	}
	return nil, fs.ErrorObjectNotFound
}

// Mkdir creates dir and any missing parents.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	abs, err := f.absolute(dir)
	if err != nil {
		return err
	}
	current := "/"
	for _, name := range strings.Split(strings.Trim(abs, "/"), "/") {
		if name == "" {
			continue
		}
		listing, err := f.client.List(ctx, current)
		if err != nil {
			return err
		}
		found := false
		for _, entry := range listing.Contents {
			if entry.Name == name {
				if !entry.IsDir {
					return fs.ErrorIsFile
				}
				found = true
				break
			}
		}
		if !found {
			if err := f.client.CreateDirectory(ctx, current, name); err != nil {
				return err
			}
		}
		current = path.Join(current, name)
	}
	return nil
}

// Rmdir moves an empty directory to Trash.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	abs, err := f.absolute(dir)
	if err != nil {
		return err
	}
	if abs == "/" {
		return fs.ErrorPermissionDenied
	}
	entries, err := f.List(ctx, dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fs.ErrorDirectoryNotEmpty
	}
	return f.client.Delete(ctx, abs)
}

// Put uploads a known-size file.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	o := &Object{f: f, remote: src.Remote()}
	if err := o.Update(ctx, in, src, options...); err != nil {
		return nil, err
	}
	return o, nil
}

// Fs returns the object's filesystem.
func (o *Object) Fs() fs.Info { return o.f }

// Remote returns the object's path relative to the configured root.
func (o *Object) Remote() string { return o.remote }

// String returns the object's remote path.
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Size returns the file size in bytes.
func (o *Object) Size() int64 { return o.size }

// ModTime returns the file's modification time.
func (o *Object) ModTime(context.Context) time.Time { return o.modTime }

// Storable reports that this object can be transferred.
func (o *Object) Storable() bool { return true }

// Hash reports that no verified remote checksum is available.
func (o *Object) Hash(context.Context, hash.Type) (string, error) { return "", hash.ErrUnsupported }

// SetModTime reports that metadata-only timestamp updates are unsupported.
func (o *Object) SetModTime(context.Context, time.Time) error { return fs.ErrorCantSetModTime }

// Remove moves the file to Trash.
func (o *Object) Remove(ctx context.Context) error {
	abs, err := o.f.fileAbsolute(o.remote)
	if err != nil {
		return err
	}
	return o.f.client.Delete(ctx, abs)
}

// Open reads the file, implementing seek and range by discarding the prefix.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	var offset int64
	limit := int64(-1)
	for _, option := range options {
		switch option := option.(type) {
		case *fs.RangeOption:
			offset, limit = option.Decode(o.size)
		case *fs.SeekOption:
			offset = option.Offset
		default:
			if option.Mandatory() {
				return nil, errors.New("idrive: unsupported mandatory download option")
			}
		}
	}
	if offset < 0 {
		return nil, errors.New("idrive: invalid download offset")
	}
	abs, err := o.f.fileAbsolute(o.remote)
	if err != nil {
		return nil, err
	}
	body, err := o.f.client.Open(ctx, abs)
	if err != nil {
		return nil, err
	}
	if offset != 0 {
		if _, err := io.CopyN(io.Discard, body, offset); err != nil {
			_ = body.Close()
			return nil, err
		}
	}
	return readers.NewLimitedReadCloser(body, limit), nil
}

// Update replaces the file contents with a known-size upload.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if src.Size() < 0 {
		return errors.New("idrive: uploads require a known size")
	}
	for _, option := range options {
		if option.Mandatory() {
			return errors.New("idrive: unsupported mandatory upload option")
		}
	}
	abs, err := o.f.fileAbsolute(o.remote)
	if err != nil {
		return err
	}
	parent := path.Dir(o.remote)
	if parent == "." {
		parent = ""
	}
	if err := o.f.Mkdir(ctx, parent); err != nil {
		return err
	}
	// IDrive skips a same-size upload if its timestamp is unchanged, even
	// when the bytes differ. Assign distinct timestamps rather than claim
	// support for preserving source modification times.
	existing, err := o.f.NewObject(ctx, o.remote)
	if err != nil && !errors.Is(err, fs.ErrorObjectNotFound) {
		return err
	}
	uploadTime := time.Now()
	if err == nil && existing.ModTime(ctx).Unix() >= uploadTime.Unix() {
		uploadTime = time.Unix(existing.ModTime(ctx).Unix()+1, 0)
	}
	if err := o.f.client.Upload(ctx, abs, in, src.Size(), uploadTime); err != nil {
		return err
	}
	obj, err := o.f.NewObject(ctx, o.remote)
	if err != nil {
		return err
	}
	*o = *obj.(*Object)
	return nil
}

var _ fs.Fs = (*Fs)(nil)
var _ fs.Object = (*Object)(nil)
