package etcd

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"time"

	"golang.org/x/net/context"

	"github.com/coreos/etcd/client"
	"github.com/luizbafilho/janus/store"
)

type Etcd struct {
	client client.KeysAPI
}

const (
	createEvent = "create"
	updateEvent = "update"
	setEvent    = "set"
	deleteEvent = "delete"
)

func New(addrs []string) store.Store {
	cfg := client.Config{
		Endpoints:               addrs,
		Transport:               client.DefaultTransport,
		HeaderTimeoutPerRequest: time.Second,
	}

	c, err := client.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	store := Etcd{
		client: client.NewKeysAPI(c),
	}

	return store
}

func (s Etcd) AddService(svc store.ServiceRequest) error {
	key := fmt.Sprintf("/fusis/services/%s-%v-%s/conf", svc.Host, svc.Port, svc.Protocol)

	exists, _ := s.keyExists(key)
	if exists {
		return fmt.Errorf("Services already exists")
	}

	value, err := json.Marshal(svc)
	if err != nil {
		return err
	}

	_, err = s.client.Set(context.Background(), key, string(value), nil)

	return err
}

func (s Etcd) UpdateService(svc store.ServiceRequest) error {
	key := fmt.Sprintf("/fusis/services/%s-%v-%s/conf", svc.Host, svc.Port, svc.Protocol)

	exists, _ := s.keyExists(key)
	if !exists {
		return fmt.Errorf("Services does not exists.")
	}

	value, err := json.Marshal(svc)
	if err != nil {
		return err
	}

	_, err = s.client.Update(context.Background(), key, string(value))

	return err
}

func (s Etcd) Subscribe(changes chan interface{}) error {
	w := s.client.Watcher("/fusis/services", &client.WatcherOptions{AfterIndex: 0, Recursive: true})

	for {
		response, err := w.Next(context.Background())
		if err != nil {
			return err
		}
		event, _ := processChange(response)

		changes <- event
	}
}

type MatcherFn func(*client.Response) (interface{}, error)

// Dispatches etcd key changes changes to the etcd to the matching functions
func processChange(response *client.Response) (interface{}, error) {
	matchers := []MatcherFn{
		processServiceChange,
	}

	for _, matcher := range matchers {
		v, err := matcher(response)
		if v != nil || err != nil {
			return v, err
		}
	}
	return nil, nil
}

func processServiceChange(r *client.Response) (interface{}, error) {
	out := regexp.MustCompile(`/fusis/services/(.*)-(\d+)-(tcp|udp)/conf`).FindStringSubmatch(r.Node.Key)

	if len(out) != 4 {
		return nil, nil
	}

	var serviceRequest store.ServiceRequest

	switch r.Action {
	case createEvent, setEvent:
		getValueFromJson(r.Node.Value, &serviceRequest)

		return store.ServiceCreate{
			Service: serviceRequest,
		}, nil

	case updateEvent:
		getValueFromJson(r.Node.Value, &serviceRequest)

		return store.ServiceUpdate{
			Service: serviceRequest,
		}, nil

	case deleteEvent:
		getValueFromJson(r.PrevNode.Value, &serviceRequest)

		return store.ServiceDelete{
			Service: serviceRequest,
		}, nil
	}

	return nil, fmt.Errorf("unsupported action on the rate: %s", r.Action)
}

func getValueFromJson(value string, v interface{}) error {
	return json.Unmarshal([]byte(value), v)
}

func (s Etcd) keyExists(key string) (bool, error) {
	_, err := s.client.Get(context.Background(), key, nil)
	if err != nil {
		if keyNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// keyNotFound checks on the error returned by the KeysAPI
// to verify if the key exists in the store or not
func keyNotFound(err error) bool {
	if err != nil {
		if etcdError, ok := err.(client.Error); ok {
			if etcdError.Code == client.ErrorCodeKeyNotFound ||
				etcdError.Code == client.ErrorCodeNotFile ||
				etcdError.Code == client.ErrorCodeNotDir {
				return true
			}
		}
	}
	return false
}
