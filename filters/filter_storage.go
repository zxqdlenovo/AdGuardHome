package filters

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AdguardTeam/AdGuardHome/util"
	"github.com/AdguardTeam/golibs/log"
	"go.uber.org/atomic"
)

// filter storage object
type filterStg struct {
	updateTaskRunning bool
	updated           []Filter  // list of filters that were downloaded during update procedure
	updateChan        chan bool // signal for the update goroutine

	conf     *Conf
	confLock sync.Mutex
	nextID   atomic.Uint64 // next filter ID

	Users []EventHandler // user handler functions for notifications
}

// initialize the module
func newFiltersObj(conf Conf) Filters {
	fs := filterStg{}
	fs.conf = &Conf{}
	*fs.conf = conf
	fs.nextID.Store(uint64(time.Now().Unix()))
	fs.updateChan = make(chan bool, 2)
	return &fs
}

// Start - start module
func (fs *filterStg) Start() {
	_ = os.MkdirAll(fs.conf.FilterDir, 0755)

	// Load all enabled filters
	// On error, RuleCount is set to 0 - users won't try to use such filters
	//  and in the future the update procedure will re-download the file
	for i := range fs.conf.List {
		f := &fs.conf.List[i]

		fname := fs.filePath(*f)
		st, err := os.Stat(fname)
		if err != nil {
			log.Debug("Filters: os.Stat: %s %s", fname, err)
			continue
		}
		f.LastUpdated = st.ModTime()

		if !f.Enabled {
			continue
		}

		file, err := os.OpenFile(fname, os.O_RDONLY, 0)
		if err != nil {
			log.Error("Filters: os.OpenFile: %s %s", fname, err)
			continue
		}

		_ = parseFilter(f, file)
		file.Close()

		f.nextUpdate = f.LastUpdated.Add(time.Duration(fs.conf.UpdateIntervalHours) * time.Hour)
	}

	if !fs.updateTaskRunning {
		fs.updateTaskRunning = true
		go fs.updateBySignal()
		go fs.updateByTimer()
	}
}

// Close - close the module
func (fs *filterStg) Close() {
	fs.updateChan <- false
	close(fs.updateChan)
}

// Duplicate filter array
func arrayFilterDup(f []Filter) []Filter {
	nf := make([]Filter, len(f))
	copy(nf, f)
	return nf
}

// WriteDiskConfig - write configuration on disk
func (fs *filterStg) WriteDiskConfig(c *Conf) {
	fs.confLock.Lock()
	*c = *fs.conf
	c.List = arrayFilterDup(fs.conf.List)
	fs.confLock.Unlock()
}

// SetConfig - set new configuration settings
func (fs *filterStg) SetConfig(c Conf) {
	fs.conf.UpdateIntervalHours = c.UpdateIntervalHours
}

// AddUser - add user handler for notifications
func (fs *filterStg) AddUser(handler EventHandler) {
	fs.Users = append(fs.Users, handler)
}

// notify all users about the event
func (fs *filterStg) notifyUsers(flags uint) {
	for _, u := range fs.Users {
		u(flags)
	}
}

// List (thread safe)
func (fs *filterStg) List(flags uint) []Filter {
	fs.confLock.Lock()
	list := make([]Filter, len(fs.conf.List))
	for i, f := range fs.conf.List {
		nf := f
		nf.Path = fs.filePath(f)
		list[i] = nf
	}
	fs.confLock.Unlock()
	return list
}

// Add - add filter (thread safe)
func (fs *filterStg) Add(nf Filter) error {
	fs.confLock.Lock()
	defer fs.confLock.Unlock()

	for _, f := range fs.conf.List {
		if f.Name == nf.Name || f.URL == nf.URL {
			return fmt.Errorf("filter with this Name or URL already exists")
		}
	}

	nf.ID = fs.nextFilterID()
	nf.Enabled = true
	err := fs.downloadFilter(&nf)
	if err != nil {
		log.Debug("%s", err)
		return err
	}
	fs.conf.List = append(fs.conf.List, nf)
	log.Debug("Filters: added filter %s", nf.URL)
	return nil
}

// Delete - remove filter (thread safe)
func (fs *filterStg) Delete(url string) *Filter {
	fs.confLock.Lock()
	defer fs.confLock.Unlock()

	nf := []Filter{}
	var found *Filter
	for i := range fs.conf.List {
		f := &fs.conf.List[i]

		if f.URL == url {
			found = f
			continue
		}
		nf = append(nf, *f)
	}
	if found == nil {
		return nil
	}
	fs.conf.List = nf
	log.Debug("Filters: removed filter %s", url)
	found.Path = fs.filePath(*found) // the caller will delete the file
	return found
}

// Modify - set filter properties (thread safe)
// Return Status* bitarray
func (fs *filterStg) Modify(url string, enabled bool, name string, newURL string) (int, Filter, error) {
	fs.confLock.Lock()
	defer fs.confLock.Unlock()

	st := 0

	for i := range fs.conf.List {
		f := &fs.conf.List[i]
		if f.URL == url {

			backup := *f
			f.Name = name

			if f.Enabled != enabled {
				f.Enabled = enabled
				st |= StatusChangedEnabled
			}

			if f.URL != newURL {
				f.URL = newURL
				st |= StatusChangedURL
			}

			needDownload := false

			if (st & StatusChangedURL) != 0 {
				f.ID = fs.nextFilterID()
				needDownload = true

			} else if (st&StatusChangedEnabled) != 0 && enabled {
				fname := fs.filePath(*f)
				file, err := os.OpenFile(fname, os.O_RDONLY, 0)
				if err != nil {
					log.Debug("Filters: os.OpenFile: %s %s", fname, err)
					needDownload = true
				} else {
					_ = parseFilter(f, file)
					file.Close()
				}
			}

			if needDownload {
				f.LastModified = ""
				f.RuleCount = 0
				err := fs.downloadFilter(f)
				if err != nil {
					*f = backup
					return 0, Filter{}, err
				}
			}

			return st, backup, nil
		}
	}

	return 0, Filter{}, fmt.Errorf("filter %s not found", url)
}

// Get filter file name
func (fs *filterStg) filePath(f Filter) string {
	return filepath.Join(fs.conf.FilterDir, fmt.Sprintf("%d.txt", f.ID))
}

// Get next filter ID
func (fs *filterStg) nextFilterID() uint64 {
	return fs.nextID.Inc()
}

// Allows printable UTF-8 text with CR, LF, TAB characters
func isPrintableText(data []byte) bool {
	for _, c := range data {
		if (c >= ' ' && c != 0x7f) || c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		return false
	}
	return true
}

// Download filter data
// Return nil on success.  Set f.Path to a file path, or "" if the file was not modified
func (fs *filterStg) downloadFilter(f *Filter) error {
	log.Debug("Filters: Downloading filter from %s", f.URL)

	// create temp file
	tmpFile, err := ioutil.TempFile(filepath.Join(fs.conf.FilterDir), "")
	if err != nil {
		return err
	}
	defer func() {
		if tmpFile != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
		}
	}()

	// create data reader object
	var reader io.Reader
	if filepath.IsAbs(f.URL) {
		f, err := os.Open(f.URL)
		if err != nil {
			return fmt.Errorf("open file: %s", err)
		}
		defer f.Close()
		reader = f
	} else {
		req, err := http.NewRequest("GET", f.URL, nil)
		if err != nil {
			return err
		}

		if len(f.LastModified) != 0 {
			req.Header.Add("If-Modified-Since", f.LastModified)
		}

		resp, err := fs.conf.HTTPClient.Do(req)
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
		}
		if err != nil {
			f.networkError = true
			return err
		}

		if resp.StatusCode == 304 { // "NOT_MODIFIED"
			log.Debug("Filters: filter %s isn't modified since %s",
				f.URL, f.LastModified)
			f.LastUpdated = time.Now()
			f.Path = ""
			return nil

		} else if resp.StatusCode != 200 {
			err := fmt.Errorf("Filters: Couldn't download filter from %s: status code: %d",
				f.URL, resp.StatusCode)
			return err
		}

		f.LastModified = resp.Header.Get("Last-Modified")

		reader = resp.Body
	}

	// parse and validate data, write to a file
	err = writeFile(f, reader, tmpFile)
	if err != nil {
		return err
	}

	// Closing the file before renaming it is necessary on Windows
	_ = tmpFile.Close()
	fname := fs.filePath(*f)
	err = os.Rename(tmpFile.Name(), fname)
	if err != nil {
		return err
	}
	tmpFile = nil // prevent from deleting this file in "defer" handler

	log.Debug("Filters: saved filter %s at %s", f.URL, fname)
	f.Path = fname
	f.LastUpdated = time.Now()
	return nil
}

func gatherUntil(dst []byte, dstLen int, src []byte, until int) int {
	num := util.MinInt(len(src), until-dstLen)
	return copy(dst[dstLen:], src[:num])
}

func isHTML(buf []byte) bool {
	s := strings.ToLower(string(buf))
	return strings.Contains(s, "<html") ||
		strings.Contains(s, "<!doctype")
}

// Read file data and count the number of rules
func parseFilter(f *Filter, reader io.Reader) error {
	ruleCount := 0
	r := bufio.NewReader(reader)

	log.Debug("Filters: parsing %s", f.URL)

	var err error
	for err == nil {
		var line string
		line, err = r.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}

		line = strings.TrimSpace(line)

		if len(line) == 0 ||
			line[0] == '#' ||
			line[0] == '!' {
			continue
		}

		ruleCount++
	}

	log.Debug("Filters: %s: %d rules", f.URL, ruleCount)

	f.RuleCount = uint64(ruleCount)
	return nil
}

// Read data, parse, write to a file
func writeFile(f *Filter, reader io.Reader, outFile *os.File) error {
	ruleCount := 0
	buf := make([]byte, 64*1024)
	total := 0
	var chunk []byte

	firstChunk := make([]byte, 4*1024)
	firstChunkLen := 0

	for {
		n, err := reader.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}
		total += n

		if !isPrintableText(buf[:n]) {
			return fmt.Errorf("data contains non-printable characters")
		}

		if firstChunk != nil {
			// gather full buffer firstChunk and perform its data tests
			firstChunkLen += gatherUntil(firstChunk, firstChunkLen, buf[:n], len(firstChunk))

			if firstChunkLen == len(firstChunk) ||
				err == io.EOF {

				if isHTML(firstChunk[:firstChunkLen]) {
					return fmt.Errorf("data is HTML, not plain text")
				}

				firstChunk = nil
			}
		}

		_, err2 := outFile.Write(buf[:n])
		if err2 != nil {
			return err2
		}

		chunk = append(chunk, buf[:n]...)
		s := string(chunk)
		for len(s) != 0 {
			i, line := splitNext(&s, '\n')
			if i < 0 && err != io.EOF {
				// no more lines in the current chunk
				break
			}
			chunk = []byte(s)

			if len(line) == 0 ||
				line[0] == '#' ||
				line[0] == '!' {
				continue
			}

			ruleCount++
		}

		if err == io.EOF {
			break
		}
	}

	log.Debug("Filters: updated filter %s: %d bytes, %d rules",
		f.URL, total, ruleCount)

	f.RuleCount = uint64(ruleCount)
	return nil
}

// SplitNext - split string by a byte
// Whitespace is trimmed
// Return byte position and the first chunk
func splitNext(data *string, by byte) (int, string) {
	s := *data
	i := strings.IndexByte(s, by)
	var chunk string
	if i < 0 {
		chunk = s
		s = ""

	} else {
		chunk = s[:i]
		s = s[i+1:]
	}

	*data = s
	chunk = strings.TrimSpace(chunk)
	return i, chunk
}

// Refresh - begin filters update procedure
func (fs *filterStg) Refresh(flags uint) {
	fs.confLock.Lock()
	defer fs.confLock.Unlock()

	for i := range fs.conf.List {
		f := &fs.conf.List[i]
		f.nextUpdate = time.Time{}
	}

	fs.updateChan <- true
}

// Start update procedure periodically
func (fs *filterStg) updateByTimer() {
	const maxPeriod = 1 * 60 * 60
	period := 5 // use a dynamically increasing time interval, while network or DNS is down
	for {
		if fs.conf.UpdateIntervalHours == 0 {
			period = maxPeriod
			// update is disabled
			time.Sleep(time.Duration(period) * time.Second)
			continue
		}

		fs.updateChan <- true

		time.Sleep(time.Duration(period) * time.Second)
		period += period
		if period > maxPeriod {
			period = maxPeriod
		}
	}
}

// Begin update procedure by signal
func (fs *filterStg) updateBySignal() {
	for {
		select {
		case ok := <-fs.updateChan:
			if !ok {
				return
			}
			fs.updateAll()
		}
	}
}

// Update filters
// Algorithm:
// . Get next filter to update:
//  . Download data from Internet and store on disk (in a new file)
//  . Add new filter to the special list
//  . Repeat for next filter
// (All filters are downloaded)
// . Stop modules that use filters
// . For each updated filter:
//  . Rename "new file name" -> "old file name"
//  . Update meta data
// . Restart modules that use filters
func (fs *filterStg) updateAll() {
	log.Debug("Filters: updating...")

	for {
		var uf Filter
		fs.confLock.Lock()
		f := fs.getNextToUpdate()
		if f != nil {
			uf = *f
		}
		fs.confLock.Unlock()

		if f == nil {
			fs.applyUpdate()
			return
		}

		uf.ID = fs.nextFilterID()
		err := fs.downloadFilter(&uf)
		if err != nil {
			if uf.networkError {
				fs.confLock.Lock()
				f.nextUpdate = time.Now().Add(10 * time.Second)
				fs.confLock.Unlock()
			}
			continue
		}

		// add new filter to the list
		fs.updated = append(fs.updated, uf)
	}
}

// Get next filter to update
func (fs *filterStg) getNextToUpdate() *Filter {
	now := time.Now()

	for i := range fs.conf.List {
		f := &fs.conf.List[i]

		if f.Enabled &&
			f.nextUpdate.Unix() <= now.Unix() {

			f.nextUpdate = now.Add(time.Duration(fs.conf.UpdateIntervalHours) * time.Hour)
			return f
		}
	}

	return nil
}

// Replace filter files
func (fs *filterStg) applyUpdate() {
	if len(fs.updated) == 0 {
		log.Debug("Filters: no filters were updated")
		return
	}

	fs.notifyUsers(EventBeforeUpdate)

	nUpdated := 0

	fs.confLock.Lock()
	for _, uf := range fs.updated {
		found := false

		for i := range fs.conf.List {
			f := &fs.conf.List[i]

			if uf.URL == f.URL {
				found = true
				fpath := fs.filePath(*f)
				f.LastUpdated = uf.LastUpdated

				if len(uf.Path) == 0 {
					// the data hasn't changed - just update file mod time
					err := os.Chtimes(fpath, f.LastUpdated, f.LastUpdated)
					if err != nil {
						log.Error("Filters: os.Chtimes: %s", err)
					}
					continue
				}

				err := os.Rename(uf.Path, fpath)
				if err != nil {
					log.Error("Filters: os.Rename:%s", err)
				}

				f.RuleCount = uf.RuleCount
				nUpdated++
				break
			}
		}

		if !found {
			// the updated filter was downloaded,
			//  but it's already removed from the main list
			_ = os.Remove(fs.filePath(uf))
		}
	}
	fs.confLock.Unlock()

	log.Debug("Filters: %d filters were updated", nUpdated)

	fs.updated = nil
	fs.notifyUsers(EventAfterUpdate)
}
