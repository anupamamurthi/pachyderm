package worker

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.pedge.io/proto/rpclog"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/pkg/hashtree"
	filesync "github.com/pachyderm/pachyderm/src/server/pkg/sync"
	ppsserver "github.com/pachyderm/pachyderm/src/server/pps"
)

type APIServer struct {
	sync.Mutex
	protorpclog.Logger
	pachClient   *client.APIClient
	etcdClient   *etcd.Client
	pipelineInfo *pps.PipelineInfo
}

func NewAPIServer(pachClient *client.APIClient, etcdClient *etcd.Client, pipelineInfo *pps.PipelineInfo) *APIServer {
	return &APIServer{
		Mutex:        sync.Mutex{},
		Logger:       protorpclog.NewLogger(""),
		pachClient:   pachClient,
		etcdClient:   etcdClient,
		pipelineInfo: pipelineInfo,
	}
}

func (a *APIServer) downloadData(ctx context.Context, data []*pfs.FileInfo) error {
	for i, datum := range data {
		input := a.pipelineInfo.Inputs[i]
		if err := filesync.Pull(ctx, a.pachClient, filepath.Join(client.PPSInputPrefix, input.Name), datum, input.Lazy); err != nil {
			return err
		}
	}
	return nil
}

func (a *APIServer) runUserCode(ctx context.Context) error {
	if err := os.MkdirAll(client.PPSOutputPath, 0666); err != nil {
		return err
	}
	transform := a.pipelineInfo.Transform
	cmd := exec.Command(transform.Cmd[0], transform.Cmd[1:]...)
	cmd.Stdin = strings.NewReader(strings.Join(transform.Stdin, "\n") + "\n")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	success := true
	if err := cmd.Run(); err != nil {
		success = false
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				for _, returnCode := range transform.AcceptReturnCode {
					if int(returnCode) == status.ExitStatus() {
						success = true
					}
				}
			}
		}
		if !success {
			fmt.Fprintf(os.Stderr, "Error from exec: %s\n", err.Error())
		}
	}
	return nil
}

func (a *APIServer) uploadOutput(ctx context.Context, data []*pfs.FileInfo) (string, error) {
	var lock sync.Mutex
	tree := hashtree.NewHashTree()
	var g errgroup.Group
	if err := filepath.Walk(client.PPSOutputPath, func(path string, info os.FileInfo, err error) error {
		g.Go(func() (retErr error) {
			if path == client.PPSOutputPath {
				return nil
			}

			relPath, err := filepath.Rel(client.PPSOutputPath, path)
			if err != nil {
				return err
			}

			if info.IsDir() {
				lock.Lock()
				defer lock.Unlock()
				tree.PutDir(relPath)
				return nil
			}

			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer func() {
				if err := f.Close(); err != nil && retErr == nil {
					retErr = err
				}
			}()

			blockRefs, err := a.pachClient.PutBlock(pfs.Delimiter_NONE, f)
			if err != nil {
				return err
			}
			lock.Lock()
			defer lock.Unlock()
			return tree.PutFile(relPath, blockRefs.BlockRef)
		})
		return nil
	}); err != nil {
		return "", err
	}

	if err := g.Wait(); err != nil {
		return "", err
	}

	finTree, err := tree.Finish()
	if err != nil {
		return "", err
	}

	treeBytes, err := hashtree.Serialize(finTree)
	if err != nil {
		return "", err
	}

	hash, err := ppsserver.HashDatum(data, a.pipelineInfo)
	if err != nil {
		return "", err
	}

	if _, err := a.pachClient.PutObject(bytes.NewReader(treeBytes), hash); err != nil {
		return "", err
	}

	return hash, nil
}

func (a *APIServer) Process(ctx context.Context, req *ProcessRequest) (resp *ProcessResponse, retErr error) {
	defer func(start time.Time) { a.Log(req, resp, retErr, time.Since(start)) }(time.Now())
	a.Lock()
	defer a.Unlock()
	if err := a.downloadData(ctx, req.Data); err != nil {
		return nil, err
	}
	if err := a.runUserCode(ctx); err != nil {
		return nil, err
	}
	tag, err := a.uploadOutput(ctx, req.Data)
	if err != nil {
		return nil, err
	}
	return &ProcessResponse{
		Tag: &pfs.Tag{tag},
	}, nil
}
