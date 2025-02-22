package app

import (
	"fmt"
	"os"
	"path"
	"strconv"

	"github.com/10gen/realm-cli/internal/cli"
	"github.com/10gen/realm-cli/internal/cli/user"
	"github.com/10gen/realm-cli/internal/cloud/atlas"
	"github.com/10gen/realm-cli/internal/cloud/realm"
	"github.com/10gen/realm-cli/internal/local"
	"github.com/10gen/realm-cli/internal/terminal"
	"github.com/10gen/realm-cli/internal/utils/flags"

	"github.com/AlecAivazis/survey/v2"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	flagLocalPathCreate      = "local"
	flagLocalPathCreateUsage = "Specify the local filepath of a Realm app to be created"

	flagCluster      = "cluster"
	flagClusterUsage = "Link Atlas cluster(s) to your Realm app"

	flagClusterServiceName      = "cluster-service-name"
	flagClusterServiceNameUsage = "Specify the Realm app Service name to reference your Atlas cluster"

	flagDatalake      = "datalake"
	flagDatalakeUsage = "Link Atlas data lake(s) to your Realm app"

	flagDatalakeServiceName      = "datalake-service-name"
	flagDatalakeServiceNameUsage = "Specify the Realm app Service name to reference your Atlas data lake"

	flagTemplate      = "template"
	flagTemplateUsage = "Create your Realm app from an available template"

	flagDryRun      = "dry-run"
	flagDryRunShort = "x"
	flagDryRunUsage = "Run without writing any changes to the local filepath or pushing any changes to the Realm server"
)

type createInputs struct {
	newAppInputs
	LocalPath            string
	Clusters             []string
	ClusterServiceNames  []string
	Datalakes            []string
	DatalakeServiceNames []string
	DryRun               bool
}

type dataSourceCluster struct {
	Name    string        `json:"name"`
	Type    string        `json:"type"`
	Config  configCluster `json:"config"`
	Version int           `json:"version"`
}

type configCluster struct {
	ClusterName         string `json:"clusterName"`
	ReadPreference      string `json:"readPreference"`
	WireProtocolEnabled bool   `json:"wireProtocolEnabled"`
}

type dataSourceDatalake struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Config  configDatalake `json:"config"`
	Version int            `json:"version"`
}

type configDatalake struct {
	DatalakeName string `json:"dataLakeName"`
}

func (i *createInputs) Resolve(profile *user.Profile, ui terminal.UI) error {
	if i.RemoteApp == "" {
		if i.Name == "" {
			if err := ui.AskOne(&i.Name, &survey.Input{Message: "App Name"}); err != nil {
				return err
			}
		}
		if i.DeploymentModel == realm.DeploymentModelEmpty {
			i.DeploymentModel = flagDeploymentModelDefault
		}
		if i.Location == realm.LocationEmpty {
			i.Location = flagLocationDefault
		}
		if i.ConfigVersion == realm.AppConfigVersionZero {
			i.ConfigVersion = realm.DefaultAppConfigVersion
		}
	}

	return nil
}

func (i *createInputs) resolveName(ui terminal.UI, client realm.Client, groupID, appNameOrClientID string) error {
	if i.Name == "" {
		app, err := cli.ResolveApp(ui, client, realm.AppFilter{
			GroupID: groupID,
			App:     appNameOrClientID,
		})
		if err != nil {
			return err
		}
		i.Name = app.Name
	}
	return nil
}

func (i *createInputs) resolveLocalPath(ui terminal.UI, wd string) (string, error) {
	//check if we are in an app directory already
	_, appOK, err := local.FindApp(wd)
	if err != nil {
		return "", err
	}
	if appOK {
		return "", errProjectExists{wd}
	}

	if i.LocalPath == "" {
		i.LocalPath = i.Name
	}
	fullPath := path.Join(wd, i.LocalPath)
	fi, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fullPath, nil
		}
		return "", err
	}
	if !fi.Mode().IsDir() {
		return fullPath, nil
	}

	defaultLocalPath := findDefaultPath(wd, i.LocalPath)
	if ui.AutoConfirm() {
		return path.Join(wd, defaultLocalPath), nil
	}

	ui.Print(terminal.NewWarningLog("Local path './%s' already exists, writing app contents to that destination may result in file conflicts.", i.LocalPath))
	proceed, err := ui.Confirm("Would you still like to write app contents to './%s'? ('No' will prompt you to provide another destination)", i.LocalPath)
	if err != nil {
		return "", err
	}
	if !proceed {
		var newDir string
		if err := ui.AskOne(&newDir, &survey.Input{Message: "Local Path", Default: defaultLocalPath}); err != nil {
			return "", err
		}

		_, appOK, err := local.FindApp(path.Join(wd, newDir))
		if err != nil {
			return "", err
		}
		if appOK {
			return "", errProjectExists{newDir}
		}

		i.LocalPath = newDir
		fullPath = path.Join(wd, i.LocalPath)
	}
	return fullPath, nil
}

func (i *createInputs) resolveClusters(ui terminal.UI, client atlas.Client, groupID string) ([]dataSourceCluster, []string, error) {
	clusters, err := client.Clusters(groupID)
	if err != nil {
		return nil, nil, err
	}

	existingClusters := map[string]struct{}{}
	for _, c := range clusters {
		existingClusters[c.Name] = struct{}{}
	}
	nonExistingClusters := make([]string, 0, len(i.Clusters))

	dsClusters := make([]dataSourceCluster, 0, len(i.Clusters))
	for idx, clusterName := range i.Clusters {
		if _, ok := existingClusters[clusterName]; !ok {
			nonExistingClusters = append(nonExistingClusters, clusterName)
			continue
		}

		serviceName := clusterName
		if len(i.ClusterServiceNames) > idx {
			serviceName = i.ClusterServiceNames[idx]
		} else {
			if !ui.AutoConfirm() {
				if err := ui.AskOne(&serviceName, &survey.Input{
					Message: fmt.Sprintf("Enter a Service Name for Cluster '%s'", clusterName),
					Default: serviceName,
				}); err != nil {
					return nil, nil, err
				}
			}
		}

		dsClusters = append(dsClusters,
			dataSourceCluster{
				Name: serviceName,
				Type: realm.ServiceTypeCluster,
				Config: configCluster{
					ClusterName:         clusterName,
					ReadPreference:      "primary",
					WireProtocolEnabled: false,
				},
				Version: 1,
			})
	}

	return dsClusters, nonExistingClusters, nil
}

func (i *createInputs) resolveDatalakes(ui terminal.UI, client atlas.Client, groupID string) ([]dataSourceDatalake, []string, error) {
	datalakes, err := client.Datalakes(groupID)
	if err != nil {
		return nil, nil, err
	}

	existingDatalakes := map[string]struct{}{}
	for _, d := range datalakes {
		existingDatalakes[d.Name] = struct{}{}
	}
	nonExistingDatalakes := make([]string, 0, len(i.Datalakes))

	dsDatalakes := make([]dataSourceDatalake, 0, len(i.Datalakes))
	for idx, datalakeName := range i.Datalakes {
		if _, ok := existingDatalakes[datalakeName]; !ok {
			nonExistingDatalakes = append(nonExistingDatalakes, datalakeName)
			continue
		}

		serviceName := datalakeName
		if len(i.DatalakeServiceNames) > idx {
			serviceName = i.DatalakeServiceNames[idx]
		} else {
			if !ui.AutoConfirm() {
				if err := ui.AskOne(&serviceName, &survey.Input{
					Message: fmt.Sprintf("Enter a Service Name for Data Lake '%s'", datalakeName),
					Default: serviceName,
				}); err != nil {
					return nil, nil, err
				}
			}
		}
		dsDatalakes = append(dsDatalakes,
			dataSourceDatalake{
				Name: serviceName,
				Type: realm.ServiceTypeDatalake,
				Config: configDatalake{
					DatalakeName: datalakeName,
				},
			})
	}

	return dsDatalakes, nonExistingDatalakes, nil
}

func (i createInputs) args(omitDryRun bool) []flags.Arg {
	args := make([]flags.Arg, 0, 8)
	if i.Project != "" {
		args = append(args, flags.Arg{flagProject, i.Project})
	}
	if i.Name != "" {
		args = append(args, flags.Arg{flagName, i.Name})
	}
	if i.RemoteApp != "" {
		args = append(args, flags.Arg{flagRemoteAppNew, i.RemoteApp})
	}
	if i.LocalPath != "" {
		args = append(args, flags.Arg{flagLocalPathCreate, i.LocalPath})
	}
	if i.Template != "" {
		args = append(args, flags.Arg{flagTemplate, i.Template})
	}
	if i.Location != flagLocationDefault {
		args = append(args, flags.Arg{flagLocation, i.Location.String()})
	}
	if i.DeploymentModel != flagDeploymentModelDefault {
		args = append(args, flags.Arg{flagDeploymentModel, i.DeploymentModel.String()})
	}
	if i.Environment != realm.EnvironmentNone {
		args = append(args, flags.Arg{flagEnvironment, i.Environment.String()})
	}
	for idx, clusterName := range i.Clusters {
		args = append(args, flags.Arg{flagCluster, clusterName})
		if len(i.ClusterServiceNames) > idx {
			args = append(args, flags.Arg{flagClusterServiceName, i.ClusterServiceNames[idx]})
		}
	}
	for idx, datalakeName := range i.Datalakes {
		args = append(args, flags.Arg{flagDatalake, datalakeName})
		if len(i.DatalakeServiceNames) > idx {
			args = append(args, flags.Arg{flagDatalakeServiceName, i.DatalakeServiceNames[idx]})
		}
	}
	if i.DryRun && !omitDryRun {
		args = append(args, flags.Arg{Name: flagDryRun})
	}
	return args
}

func findDefaultPath(wd string, localPath string) string {
	for i := 1; i < 10; i++ {
		newPath := localPath + "-" + strconv.Itoa(i)
		_, found, err := local.FindApp(path.Join(wd, newPath))
		if err == nil && !found {
			return newPath
		}
	}
	return localPath + "-" + primitive.NewObjectID().Hex()
}
