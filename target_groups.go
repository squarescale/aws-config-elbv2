package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path"
	"sync"

	etcd "github.com/coreos/etcd/client"
)

type TargetGroups struct {
	lock         sync.Mutex
	ctx          context.Context
	etcd         etcd.Client
	TargetGroups map[string]*TargetGroup
}

type TargetGroup struct {
	etcd        etcd.Client
	lock        sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	Name        string
	Arn         string
	Attachments map[string]*TGAttachment
}

type TGAttachment struct {
	Id   string `json:"id"`
	Port int    `json:"port"`
}

func NewTargetGroups(ctx context.Context, wg sync.WaitGroup, etcd_endpoints []string) (*TargetGroups, error) {
	var err error
	list := new(TargetGroups)
	list.TargetGroups = map[string]*TargetGroup{}
	list.etcd, err = etcd.New(etcd.Config{
		Endpoints: etcd_endpoints,
	})
	if err != nil {
		return nil, err
	}
	list.ctx = ctx
	err = list.watchEtcd(ctx, wg)
	return list, err
}

func (tg *TargetGroups) watchEtcd(ctx context.Context, wg sync.WaitGroup) error {
	dir := "/aws-config-elbv2/target-group"

	keys := etcd.NewKeysAPI(tg.etcd)

	res, err := keys.Get(ctx, dir, &etcd.GetOptions{
		Recursive: true,
	})
	if err != nil && !etcd.IsKeyNotFound(err) {
		return err
	} else if err == nil && res != nil && res.Node != nil && res.Node.Dir {
		for _, targetGroup := range res.Node.Nodes {
			tg.addTargetGroup(ctx, wg, targetGroup)
		}
	}

	var index uint64
	if err != nil {
		e := err.(etcd.Error)
		index = e.Index
	} else {
		index = res.Index
	}

	go func() {
		wg.Add(1)
		defer wg.Done()

		var watch etcd.Watcher
		for ctx.Err() == nil {
			if watch == nil {
				watch = keys.Watcher(dir, &etcd.WatcherOptions{
					AfterIndex: index,
					Recursive:  true,
				})
			}

			res, err := watch.Next(ctx)
			if err != nil {
				if e, ok := err.(etcd.Error); ok {
					watch = nil
					index = e.Index
				}
				continue
			}

			if res != nil && res.Node.Key == path.Join(dir, path.Base(res.Node.Key)) {
				log.Printf("{%v} %v %v", dir, res.Action, res.Node.Key)
				var err error = nil
				if res.Action == "set" || res.Action == "create" {
					log.Printf("etcd: %s %s", res.Action, res.Node.Key)
					err = tg.addTargetGroup(ctx, wg, res.Node)
				} else if res.Action == "delete" {
					log.Printf("etcd: %s %s", res.Action, res.Node.Key)
					err = tg.removeTargetGroup(ctx, res.PrevNode)
				}
				if err != nil {
					log.Printf("etcd error: %s", err)
				}
			}
		}
	}()
	return nil
}

func (tgs *TargetGroups) addTargetGroup(ctx context.Context, wg sync.WaitGroup, node *etcd.Node) error {
	var arn string

	tgs.lock.Lock()
	defer tgs.lock.Unlock()

	name := path.Base(node.Key)

	for _, n := range node.Nodes {
		if path.Base(n.Key) == "arn" && !n.Dir {
			arn = n.Value
		}
	}

	tg, ok := tgs.TargetGroups[name]
	if ok {
		tg.lock.Lock()
		defer tg.lock.Unlock()
		log.Printf("[%s] Set Target Group ARN to %#s", node.Key, arn)
		tg.Arn = arn
		return nil
	}

	if arn != "" {
		log.Printf("[%s] Add Target Group with ARN %#s", node.Key, arn)
	} else {
		log.Printf("[%s] Add Target Group", node.Key)
	}

	ctx2, cancel := context.WithCancel(ctx)
	tg = &TargetGroup{
		Name:        name,
		Arn:         arn,
		ctx:         ctx2,
		cancel:      cancel,
		Attachments: map[string]*TGAttachment{},
		etcd:        tgs.etcd,
	}

	tgs.TargetGroups[name] = tg
	go tg.Watch(ctx2, wg)
	return nil
}

func (tgs *TargetGroups) removeTargetGroup(ctx context.Context, node *etcd.Node) error {
	tgs.lock.Lock()
	defer tgs.lock.Unlock()

	log.Printf("[%s] Remove target group", node.Key)

	name := path.Base(node.Key)
	tg, ok := tgs.TargetGroups[name]
	if ok {
		tg.cancel()
		tg.removeAllAttachments(ctx)
		delete(tgs.TargetGroups, name)
	}
	return nil
}

func (tg *TargetGroup) Watch(ctx context.Context, wg sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()

	dir := "/aws-config-elbv2/target-group/" + tg.Name + "/attachments"

	var watch etcd.Watcher
	keys := etcd.NewKeysAPI(tg.etcd)

	res, err := keys.Get(ctx, dir, &etcd.GetOptions{})
	var index uint64
	if err != nil {
		e := err.(etcd.Error)
		index = e.Index
	} else {
		index = res.Index
	}

	for ctx.Err() == nil {
		if watch == nil {
			watch = keys.Watcher(dir, &etcd.WatcherOptions{
				AfterIndex: index,
				Recursive:  true,
			})
		}

		res, err := watch.Next(ctx)
		if err != nil {
			if e, ok := err.(etcd.Error); ok {
				watch = nil
				index = e.Index
			}
			continue
		}

		if res != nil && res.Node.Key == path.Join(dir, path.Base(res.Node.Key)) {
			log.Printf("{%v} %v %v", dir, res.Action, res.Node.Key)
			var err error = nil
			if res.Action == "set" {
				log.Printf("etcd: %s %s", res.Action, res.Node.Key)
				err = tg.removeTargetGroupAttachment(ctx, res.PrevNode)
				err = tg.addTargetGroupAttachment(ctx, res.Node)
			} else if res.Action == "create" {
				log.Printf("etcd: %s %s", res.Action, res.Node.Key)
				err = tg.addTargetGroupAttachment(ctx, res.Node)
			} else if res.Action == "delete" {
				log.Printf("etcd: %s %s", res.Action, res.Node.Key)
				err = tg.removeTargetGroupAttachment(ctx, res.PrevNode)
			}
			if err != nil {
				log.Printf("etcd error: %s", err)
			}
		}
	}
}

func (tg *TargetGroup) addTargetGroupAttachment(ctx context.Context, node *etcd.Node) error {
	tg.lock.Lock()
	defer tg.lock.Unlock()

	name := path.Base(node.Key)
	log.Printf("[%s] Add attachment", node.Key)

	var attach TGAttachment
	err := json.Unmarshal([]byte(node.Value), &attach)
	if err != nil {
		return err
	}

	log.Printf("[%s] + TG ARN: %s", node.Key, tg.Arn)
	log.Printf("[%s] + Id:     %s", node.Key, attach.Id)
	log.Printf("[%s] + Port:   %d", node.Key, attach.Port)

	tg.Attachments[name] = &attach
	return nil
}

func (tg *TargetGroup) removeAllAttachments(ctx context.Context) []error {
	tg.lock.Lock()
	defer tg.lock.Unlock()
	var errs []error = nil
	for name := range tg.Attachments {
		err := tg.removeAttachment(ctx, name)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (tg *TargetGroup) removeTargetGroupAttachment(ctx context.Context, node *etcd.Node) error {
	tg.lock.Lock()
	defer tg.lock.Unlock()
	name := path.Base(node.Key)
	return tg.removeAttachment(ctx, name)
}

func (tg *TargetGroup) removeAttachment(ctx context.Context, name string) error {
	key := "/aws-config-elbv2/target-group/" + tg.Name + "/attachments/" + name
	log.Printf("[%s] Remove attachment", key)

	attach, ok := tg.Attachments[name]
	if ok {
		delete(tg.Attachments, name)
		log.Printf("[%s] - TG ARN: %s", key, tg.Arn)
		log.Printf("[%s] - Id:     %s", key, attach.Id)
		log.Printf("[%s] - Port:   %d", key, attach.Port)
	}
	return nil
}

func (tgs *TargetGroups) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	encoder := json.NewEncoder(w)

	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		encoder.Encode(tgs)

	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}
