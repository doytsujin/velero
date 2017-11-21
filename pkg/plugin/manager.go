/*
Copyright 2017 the Heptio Ark contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package plugin

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/heptio/ark/pkg/backup"
	"github.com/heptio/ark/pkg/cloudprovider"
)

// PluginKind is a type alias for a string that describes
// the kind of an Ark-supported plugin.
type PluginKind string

func (k PluginKind) String() string {
	return string(k)
}

func baseConfig() *plugin.ClientConfig {
	return &plugin.ClientConfig{
		HandshakeConfig:  Handshake,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
	}
}

const (
	// PluginKindObjectStore is the Kind string for
	// an Object Store plugin.
	PluginKindObjectStore PluginKind = "objectstore"

	// PluginKindBlockStore is the Kind string for
	// a Block Store plugin.
	PluginKindBlockStore PluginKind = "blockstore"

	// PluginKindCloudProvider is the Kind string for
	// a CloudProvider plugin (i.e. an Object & Block
	// store).
	//
	// NOTE that it is highly likely that in subsequent
	// versions of Ark this kind of plugin will be replaced
	// with a different mechanism for providing multiple
	// plugin impls within a single binary. This should
	// probably not be used.
	PluginKindCloudProvider PluginKind = "cloudprovider"

	// PluginKindBackupItemAction is the Kind string for
	// a Backup ItemAction plugin.
	PluginKindBackupItemAction PluginKind = "backupitemaction"

	pluginDir = "/plugins"
)

var AllPluginKinds = []PluginKind{
	PluginKindObjectStore,
	PluginKindBlockStore,
	PluginKindCloudProvider,
	PluginKindBackupItemAction,
}

type pluginInfo struct {
	kinds       []PluginKind
	name        string
	commandName string
	commandArgs []string
}

// Manager exposes functions for getting implementations of the pluggable
// Ark interfaces.
type Manager interface {
	// GetObjectStore returns the plugin implementation of the
	// cloudprovider.ObjectStore interface with the specified name.
	GetObjectStore(name string) (cloudprovider.ObjectStore, error)

	// GetBlockStore returns the plugin implementation of the
	// cloudprovider.BlockStore interface with the specified name.
	GetBlockStore(name string) (cloudprovider.BlockStore, error)

	// GetBackupItemActions returns all backup.ItemAction plugins.
	// These plugin instances should ONLY be used for a single backup
	// (mainly because each one outputs to a per-backup log),
	// and should be terminated upon completion of the backup with
	// CloseBackupItemActions().
	GetBackupItemActions(backupName string, logger logrus.FieldLogger, level logrus.Level) ([]backup.ItemAction, error)

	// CloseBackupItemActions terminates the plugin sub-processes that
	// are hosting BackupItemAction plugins for the given backup name.
	CloseBackupItemActions(backupName string) error
}

type manager struct {
	logger         hclog.Logger
	pluginRegistry *registry
	clientStore    *clientStore
}

// NewManager constructs a manager for getting plugin implementations.
func NewManager(logger logrus.FieldLogger, level logrus.Level) (Manager, error) {
	m := &manager{
		logger:         &logrusAdapter{impl: logger, level: level},
		pluginRegistry: newRegistry(),
		clientStore:    newClientStore(),
	}

	if err := m.registerPlugins(); err != nil {
		return nil, err
	}

	return m, nil
}

func pluginForKind(kind PluginKind) plugin.Plugin {
	switch kind {
	case PluginKindObjectStore:
		return &ObjectStorePlugin{}
	case PluginKindBlockStore:
		return &BlockStorePlugin{}
	default:
		return nil
	}
}

func getPluginInstance(client *plugin.Client, kind PluginKind) (interface{}, error) {
	protocolClient, err := client.Client()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	plugin, err := protocolClient.Dispense(string(kind))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return plugin, nil
}

func (m *manager) registerPlugins() error {
	// first, register internal plugins
	for _, provider := range []string{"aws", "gcp", "azure"} {
		m.pluginRegistry.register(provider, "/ark", []string{"plugin", "cloudprovider", provider}, PluginKindObjectStore, PluginKindBlockStore)
	}
	m.pluginRegistry.register("backup_pv", "/ark", []string{"plugin", string(PluginKindBackupItemAction), "backup_pv"}, PluginKindBackupItemAction)

	// second, register external plugins (these will override internal plugins, if applicable)
	if _, err := os.Stat(pluginDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	files, err := ioutil.ReadDir(pluginDir)
	if err != nil {
		return err
	}

	for _, file := range files {
		name, kind, err := parse(file.Name())
		if err != nil {
			continue
		}

		if kind == PluginKindCloudProvider {
			m.pluginRegistry.register(name, filepath.Join(pluginDir, file.Name()), nil, PluginKindObjectStore, PluginKindBlockStore)
		} else {
			m.pluginRegistry.register(name, filepath.Join(pluginDir, file.Name()), nil, kind)
		}
	}

	return nil
}

func parse(filename string) (string, PluginKind, error) {
	for _, kind := range AllPluginKinds {
		if prefix := fmt.Sprintf("ark-%s-", kind); strings.Index(filename, prefix) == 0 {
			return strings.Replace(filename, prefix, "", -1), kind, nil
		}
	}

	return "", "", errors.New("invalid file name")
}

// GetObjectStore returns the plugin implementation of the cloudprovider.ObjectStore
// interface with the specified name.
func (m *manager) GetObjectStore(name string) (cloudprovider.ObjectStore, error) {
	pluginObj, err := m.getCloudProviderPlugin(name, PluginKindObjectStore)
	if err != nil {
		return nil, err
	}

	objStore, ok := pluginObj.(cloudprovider.ObjectStore)
	if !ok {
		return nil, errors.New("could not convert gRPC client to cloudprovider.ObjectStore")
	}

	return objStore, nil
}

// GetBlockStore returns the plugin implementation of the cloudprovider.BlockStore
// interface with the specified name.
func (m *manager) GetBlockStore(name string) (cloudprovider.BlockStore, error) {
	pluginObj, err := m.getCloudProviderPlugin(name, PluginKindBlockStore)
	if err != nil {
		return nil, err
	}

	blockStore, ok := pluginObj.(cloudprovider.BlockStore)
	if !ok {
		return nil, errors.New("could not convert gRPC client to cloudprovider.BlockStore")
	}

	return blockStore, nil
}

func (m *manager) getCloudProviderPlugin(name string, kind PluginKind) (interface{}, error) {
	client, err := m.clientStore.get(kind, name, "")
	if err != nil {
		pluginInfo, err := m.pluginRegistry.get(kind, name)
		if err != nil {
			return nil, err
		}

		// build a plugin client that can dispense all of the PluginKinds it's registered for
		clientBuilder := newClientBuilder(baseConfig()).
			withCommand(pluginInfo.commandName, pluginInfo.commandArgs...)

		for _, kind := range pluginInfo.kinds {
			clientBuilder.withPlugin(kind, pluginForKind(kind))
		}

		client = clientBuilder.client()

		// register the plugin client for the appropriate kinds
		for _, kind := range pluginInfo.kinds {
			m.clientStore.add(client, kind, name, "")
		}
	}

	pluginObj, err := getPluginInstance(client, kind)
	if err != nil {
		return nil, err
	}

	return pluginObj, nil
}

// GetBackupActions returns all backup.BackupAction plugins.
// These plugin instances should ONLY be used for a single backup
// (mainly because each one outputs to a per-backup log),
// and should be terminated upon completion of the backup with
// CloseBackupActions().
func (m *manager) GetBackupItemActions(backupName string, logger logrus.FieldLogger, level logrus.Level) ([]backup.ItemAction, error) {
	clients, err := m.clientStore.list(PluginKindBackupItemAction, backupName)
	if err != nil {
		pluginInfo, err := m.pluginRegistry.list(PluginKindBackupItemAction)
		if err != nil {
			return nil, err
		}

		// create clients for each, using the provided logger
		log := &logrusAdapter{impl: logger, level: level}

		for _, plugin := range pluginInfo {
			client := newClientBuilder(baseConfig()).
				withCommand(plugin.commandName, plugin.commandArgs...).
				withPlugin(PluginKindBackupItemAction, &BackupItemActionPlugin{log: log}).
				withLogger(log).
				client()

			m.clientStore.add(client, PluginKindBackupItemAction, plugin.name, backupName)

			clients = append(clients, client)
		}
	}

	var backupActions []backup.ItemAction
	for _, client := range clients {
		plugin, err := getPluginInstance(client, PluginKindBackupItemAction)
		if err != nil {
			return nil, err
		}

		backupAction, ok := plugin.(backup.ItemAction)
		if !ok {
			return nil, errors.New("could not convert gRPC client to backup.BackupAction")
		}

		backupActions = append(backupActions, backupAction)
	}

	return backupActions, nil
}

// CloseBackupItemActions terminates the plugin sub-processes that
// are hosting BackupItemAction plugins for the given backup name.
func (m *manager) CloseBackupItemActions(backupName string) error {
	clients, err := m.clientStore.list(PluginKindBackupItemAction, backupName)
	if err != nil {
		return err
	}

	for _, client := range clients {
		client.Kill()
	}

	m.clientStore.deleteAll(PluginKindBackupItemAction, backupName)

	return nil
}
