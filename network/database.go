package network

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/Safing/portbase/database"
	"github.com/Safing/portbase/database/iterator"
	"github.com/Safing/portbase/database/query"
	"github.com/Safing/portbase/database/record"
	"github.com/Safing/portbase/database/storage"
	"github.com/Safing/portmaster/process"
)

var (
	links     = make(map[string]*Link) // key: Link ID
	linksLock sync.RWMutex
	comms     = make(map[string]*Communication) //  key: PID/Domain
	commsLock sync.RWMutex

	dbController *database.Controller
)

// StorageInterface provices a storage.Interface to the configuration manager.
type StorageInterface struct {
	storage.InjectBase
}

// Get returns a database record.
func (s *StorageInterface) Get(key string) (record.Record, error) {

	splitted := strings.Split(key, "/")
	switch splitted[0] {
	case "tree":
		switch len(splitted) {
		case 2:
			pid, err := strconv.Atoi(splitted[1])
			if err == nil {
				proc, ok := process.GetProcessFromStorage(pid)
				if ok {
					return proc, nil
				}
			}
		case 3:
			commsLock.RLock()
			defer commsLock.RUnlock()
			conn, ok := comms[fmt.Sprintf("%s/%s", splitted[1], splitted[2])]
			if ok {
				return conn, nil
			}
		case 4:
			linksLock.RLock()
			defer linksLock.RUnlock()
			link, ok := links[splitted[3]]
			if ok {
				return link, nil
			}
		}
	}

	return nil, storage.ErrNotFound
}

// Query returns a an iterator for the supplied query.
func (s *StorageInterface) Query(q *query.Query, local, internal bool) (*iterator.Iterator, error) {
	it := iterator.New()
	go s.processQuery(q, it)
	// TODO: check local and internal

	return it, nil
}

func (s *StorageInterface) processQuery(q *query.Query, it *iterator.Iterator) {
	slashes := strings.Count(q.DatabaseKeyPrefix(), "/")

	if slashes <= 1 {
		// processes
		for _, proc := range process.All() {
			if strings.HasPrefix(proc.DatabaseKey(), q.DatabaseKeyPrefix()) {
				it.Next <- proc
			}
		}
	}

	if slashes <= 2 {
		// comms
		commsLock.RLock()
		for _, conn := range comms {
			if strings.HasPrefix(conn.DatabaseKey(), q.DatabaseKeyPrefix()) {
				it.Next <- conn
			}
		}
		commsLock.RUnlock()
	}

	if slashes <= 3 {
		// links
		linksLock.RLock()
		for _, link := range links {
			if strings.HasPrefix(link.DatabaseKey(), q.DatabaseKeyPrefix()) {
				it.Next <- link
			}
		}
		linksLock.RUnlock()
	}

	it.Finish(nil)
}

func registerAsDatabase() error {
	_, err := database.Register(&database.Database{
		Name:        "network",
		Description: "Network and Firewall Data",
		StorageType: "injected",
		PrimaryAPI:  "",
	})
	if err != nil {
		return err
	}

	controller, err := database.InjectDatabase("network", &StorageInterface{})
	if err != nil {
		return err
	}

	dbController = controller
	process.SetDBController(dbController)
	return nil
}
