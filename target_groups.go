package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
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
	Region      string
	Attachments map[string]*TGAttachment
}

type TGAttachment struct {
	Id   string `json:"id"`
	Port int64  `json:"port"`
}

func NewTargetGroups(ctx context.Context, wg *sync.WaitGroup, etcd_endpoints []string) (*TargetGroups, error) {
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
	list.watchEtcd(ctx, wg)
	return list, nil
}

func (tgs *TargetGroups) watchEtcd(ctx context.Context, wg *sync.WaitGroup) {
	dir := "/aws-config-elbv2/target-group"

	keys := etcd.NewKeysAPI(tgs.etcd)

	res, err := keys.Get(ctx, dir, &etcd.GetOptions{
		Recursive: true,
	})
	if err != nil && !etcd.IsKeyNotFound(err) {
		log.Printf("{%v} error: %s", dir, err)
	} else if err == nil && res != nil && res.Node != nil && res.Node.Dir {
		for _, targetGroup := range res.Node.Nodes {
			log.Printf("{%v} get %v", dir, path.Base(targetGroup.Key))
			tgs.addTargetGroup(ctx, wg, targetGroup)
		}
	}

	var index uint64
	if err != nil {
		e := err.(etcd.Error)
		index = e.Index
	} else {
		index = res.Index
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Printf("{%v} watching index %d", dir, index)
		defer func() { log.Printf("{%s} unwatch: %v", dir, ctx.Err()) }()

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

			if res != nil {
				var relpath, tgname string
				if res.Node.Key != dir {
					relpath = res.Node.Key[len(dir)+1:]
				}
				if elems := strings.Split(relpath, "/"); len(elems) > 0 {
					tgname = elems[0]
					if len(relpath) > len(tgname) {
						relpath = relpath[len(tgname)+1:]
					} else {
						relpath = ""
					}
				}
				log.Printf("{%v} %v %v/%v", dir, res.Action, tgname, relpath)

				if tgname != "" {
					res2, err := keys.Get(ctx, path.Join(dir, tgname), &etcd.GetOptions{
						Recursive: true,
					})
					if err != nil && !etcd.IsKeyNotFound(err) {
						log.Printf("{%v} error %s: %s", dir, tgname, err)
						err = nil
					} else if err != nil && etcd.IsKeyNotFound(err) {
						err = tgs.removeTargetGroup(ctx, path.Join(dir, tgname))
					} else if err == nil && res2 != nil {
						tg, ok := tgs.TargetGroups[tgname]
						if ok {
							tg.UpdateTargetGroup(ctx, wg, res2.Node)
						} else {
							err = tgs.addTargetGroup(ctx, wg, res2.Node)
						}
					}
					if err != nil {
						log.Printf("{%v} error %s: %s", dir, tgname, err)
					}
				}
			}
		}
	}()
}

func (tgs *TargetGroups) addTargetGroup(ctx context.Context, wg *sync.WaitGroup, node *etcd.Node) error {
	tgs.lock.Lock()
	defer tgs.lock.Unlock()

	name := path.Base(node.Key)

	tg, ok := tgs.TargetGroups[name]
	if ok {
		return tg.UpdateTargetGroup(ctx, wg, node)
	}

	log.Printf("[%s] Add Target Group", node.Key)

	ctx2, cancel := context.WithCancel(ctx)
	tg = &TargetGroup{
		Name:        name,
		Arn:         "",
		Region:      "",
		ctx:         ctx2,
		cancel:      cancel,
		Attachments: map[string]*TGAttachment{},
		etcd:        tgs.etcd,
	}
	err := tg.UpdateTargetGroup(ctx2, wg, node)

	tgs.TargetGroups[name] = tg
	tg.StartWatch(ctx2, wg)
	return err
}

func (tgs *TargetGroups) removeTargetGroup(ctx context.Context, nodeKey string) error {
	tgs.lock.Lock()
	defer tgs.lock.Unlock()

	log.Printf("[%s] Remove target group", nodeKey)

	name := path.Base(nodeKey)
	tg, ok := tgs.TargetGroups[name]
	if ok {
		tg.cancel()
		tg.removeAllAttachments(ctx)
		delete(tgs.TargetGroups, name)
	}
	return nil
}

func (tg *TargetGroup) UpdateTargetGroup(ctx context.Context, wg *sync.WaitGroup, node *etcd.Node) error {
	var arn string
	var region string

	for _, n := range node.Nodes {
		if path.Base(n.Key) == "arn" && !n.Dir {
			arn = n.Value
		} else if path.Base(n.Key) == "region" && !n.Dir {
			region = n.Value
		}
	}

	tg.lock.Lock()
	defer tg.lock.Unlock()

	if arn != "" && arn != tg.Arn {
		log.Printf("[%s] ARN: %#s", node.Key, arn)
		tg.Arn = arn
	}
	if region != "" && region != tg.Region {
		log.Printf("[%s] Region: %#s", node.Key, region)
		tg.Region = region
	}
	return nil
}

func (tg *TargetGroup) StartWatch(ctx context.Context, wg *sync.WaitGroup) {
	dir := "/aws-config-elbv2/target-group/" + tg.Name + "/attachments"

	var watch etcd.Watcher
	keys := etcd.NewKeysAPI(tg.etcd)

	res, err := keys.Get(ctx, dir, &etcd.GetOptions{
		Recursive: true,
	})
	if err != nil && !etcd.IsKeyNotFound(err) {
		log.Printf("{%v} error: %s", dir, err)
	} else if err == nil && res != nil && res.Node != nil && res.Node.Dir {
		for _, attach := range res.Node.Nodes {
			log.Printf("{%v} get %v", dir, path.Base(attach.Key))
			err = tg.addTargetGroupAttachment(ctx, attach)
			if err != nil {
				log.Printf("{%v} error: %s", attach.Key, err)
			}
		}
	}

	var index uint64
	if err != nil {
		e := err.(etcd.Error)
		index = e.Index
	} else {
		index = res.Index
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Printf("{%v} watching index %d", dir, index)
		defer func() { log.Printf("{%s} unwatch: %v", dir, ctx.Err()) }()

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
				name := path.Base(res.Node.Key)
				log.Printf("{%v} %v %v", dir, res.Action, name)
				var err error = nil
				if res.Action == "set" {
					err = tg.addTargetGroupAttachment(ctx, res.Node)
				} else if res.Action == "create" {
					err = tg.addTargetGroupAttachment(ctx, res.Node)
				} else if res.Action == "delete" {
					err = tg.removeTargetGroupAttachment(ctx, name)
				}
				if err != nil {
					log.Printf("{%v} error: %s", dir, err)
				}
			}
		}
	}()
}

func (tg *TargetGroup) addTargetGroupAttachment(ctx context.Context, node *etcd.Node) error {
	tg.lock.Lock()
	defer tg.lock.Unlock()

	var attach TGAttachment
	err := json.Unmarshal([]byte(node.Value), &attach)
	if err != nil {
		return err
	}

	name := path.Base(node.Key)
	oldAttach := tg.Attachments[name]
	if oldAttach != nil && oldAttach.Id == attach.Id && oldAttach.Port == attach.Port {
		return nil
	}

	log.Printf("[%s] Add attachment", node.Key)
	log.Printf("[%s] + TG ARN: %s", node.Key, tg.Arn)
	log.Printf("[%s] + Id:     %s", node.Key, attach.Id)
	log.Printf("[%s] + Port:   %d", node.Key, attach.Port)

	sess := awsSession
	if tg.Region != "" {
		sess = sess.Copy(&aws.Config{Region: aws.String(tg.Region)})
	}
	svc := elbv2.New(sess)

	resp, err := svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(tg.Arn),
		Targets: []*elbv2.TargetDescription{
			{
				Id:   aws.String(attach.Id),
				Port: aws.Int64(attach.Port),
			},
		},
	})

	_ = resp
	if err != nil {
		log.Printf("[%s] error: %v", node.Key, err)
	}

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

func (tg *TargetGroup) removeTargetGroupAttachment(ctx context.Context, name string) error {
	tg.lock.Lock()
	defer tg.lock.Unlock()
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

		sess := awsSession
		if tg.Region != "" {
			sess = sess.Copy(&aws.Config{Region: aws.String(tg.Region)})
		}
		svc := elbv2.New(sess)

		resp, err := svc.DeregisterTargets(&elbv2.DeregisterTargetsInput{
			TargetGroupArn: aws.String(tg.Arn),
			Targets: []*elbv2.TargetDescription{
				{
					Id:   aws.String(attach.Id),
					Port: aws.Int64(attach.Port),
				},
			},
		})

		_ = resp
		if err != nil {
			log.Printf("[%s] error: %v", key, err)
		}
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
