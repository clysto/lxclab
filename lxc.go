package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
	"github.com/gorilla/websocket"
	"github.com/lithammer/shortuuid/v4"
)

type LXCClient struct {
	client         lxd.InstanceServer
	usedPorts      map[int]bool
	mutex          sync.Mutex
	defaultProfile string
}

func NewLXCClient(defaultProfile string) (*LXCClient, error) {
	client, err := lxd.ConnectLXDUnix("", nil)
	if err != nil {
		return nil, err
	}
	usedPorts := make(map[int]bool)
	containers, err := client.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return nil, err
	}
	// Scan all ssh ports
	for _, container := range containers {
		for name, device := range container.Devices {
			if name == "port22" && device["type"] == "proxy" {
				listen := device["listen"]
				parsed := strings.Split(listen, ":")
				if len(parsed) > 0 {
					port, err := strconv.Atoi(parsed[len(parsed)-1])
					if err == nil {
						usedPorts[port] = true
					}
				}
			}
		}
	}
	return &LXCClient{
		client:         client,
		usedPorts:      usedPorts,
		mutex:          sync.Mutex{},
		defaultProfile: defaultProfile,
	}, nil
}

func (c *LXCClient) ListContainers(username string) ([]api.Instance, error) {
	containers, err := c.client.GetInstancesWithFilter(api.InstanceTypeContainer, []string{"config.user.username=" + username})
	if err != nil {
		return nil, err
	}
	return containers, nil
}

func (c *LXCClient) GetContainer(username string, name string) (*api.Instance, error) {
	containers, err := c.ListContainers(username)
	if err != nil {
		return nil, err
	}
	for _, container := range containers {
		if container.Name == name {
			return &container, nil
		}
	}
	return nil, nil
}

func (c *LXCClient) CreateContainer(username string, friendlyname string) error {
	instancePost := api.InstancesPost{
		Name: shortuuid.New(),
		Source: api.InstanceSource{
			Type:              "image",
			Fingerprint:       "c9fba5728bfe168aa73084b94deab3dd3a1e349b5f7e0b5e5a8e945899cb0378",
			AllowInconsistent: false,
		},
		InstancePut: api.InstancePut{
			Profiles: []string{
				c.defaultProfile,
			},
			Config: map[string]string{
				"user.username":     username,
				"user.friendlyname": friendlyname,
			},
			Devices: map[string]map[string]string{},
		},
	}

	// Find an unused port for SSH
	sshPort, err := c.UnusedPort(22000, 23000)
	if err == nil {
		instancePost.Devices["port22"] = map[string]string{
			"type":    "proxy",
			"connect": "tcp:127.0.0.1:22",
			"listen":  "tcp:0.0.0.0:" + strconv.Itoa(sshPort),
		}
	}

	op, err := c.client.CreateInstance(instancePost)
	if err != nil {
		return err
	}

	return op.Wait()
}

func (c *LXCClient) DeleteContainer(username string, name string) error {
	container, err := c.GetContainer(username, name)
	if err != nil {
		return err
	}
	if container == nil {
		return nil
	}
	sshPort := c.SSHPort(container.Name)
	op, err := c.client.DeleteInstance(container.Name)
	if err != nil {
		return err
	}
	err = op.Wait()
	if err != nil {
		return err
	}
	// Release ssh port
	if sshPort > 0 {
		c.ReleasePort(sshPort)
	}
	return nil
}

func (c *LXCClient) StartContainer(username string, name string) error {
	container, err := c.GetContainer(username, name)
	if err != nil {
		return err
	}
	if container == nil {
		return nil
	}
	op, err := c.client.UpdateInstanceState(container.Name, api.InstanceStatePut{
		Action: "start",
	}, "")

	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *LXCClient) StopContainer(username string, name string) error {
	container, err := c.GetContainer(username, name)
	if err != nil {
		return err
	}
	if container == nil {
		return nil
	}
	op, err := c.client.UpdateInstanceState(container.Name, api.InstanceStatePut{
		Action: "stop",
	}, "")
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *LXCClient) StartShell(name string, stdin io.Reader, stdout io.Writer, ch chan [2]int, dataDone chan bool) error {
	op, err := c.client.ExecInstance(name, api.InstanceExecPost{
		Command: []string{"bash"},
		Environment: map[string]string{
			"TERM": "xterm-256color",
			"HOME": "/home/ubuntu",
		},
		User:        1000,
		Group:       1000,
		WaitForWS:   true,
		Width:       80,
		Height:      24,
		Interactive: true,
	}, &lxd.InstanceExecArgs{
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stdout,
		DataDone: dataDone,
		Control: func(conn *websocket.Conn) {
			for {
				sig := <-ch
				width, height := sig[0], sig[1]
				msg := api.InstanceExecControl{}
				msg.Command = "window-resize"
				msg.Args = make(map[string]string)
				msg.Args["width"] = strconv.Itoa(width)
				msg.Args["height"] = strconv.Itoa(height)
				conn.WriteJSON(msg)
			}
		},
	})
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *LXCClient) SSHPort(name string) int {
	container, _, err := c.client.GetInstance(name)
	if err != nil {
		return 0
	}
	for name, device := range container.Devices {
		if name == "port22" && device["type"] == "proxy" {
			listen := device["listen"]
			parsed := strings.Split(listen, ":")
			if len(parsed) > 0 {
				port, err := strconv.Atoi(parsed[len(parsed)-1])
				if err == nil {
					return port
				}
			}
		}
	}
	return 0
}

func (c *LXCClient) ReleasePort(port int) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.usedPorts[port] = false
}

func (c *LXCClient) UnusedPort(low int, high int) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	for port := low; port < high; port++ {
		if !c.usedPorts[port] {
			c.usedPorts[port] = true
			return port, nil
		}
	}
	return 0, fmt.Errorf("no unused port found")
}
