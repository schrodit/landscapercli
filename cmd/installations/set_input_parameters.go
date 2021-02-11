package installations

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	lsv1alpha1 "github.com/gardener/landscaper/apis/core/v1alpha1"
	"github.com/gardener/landscaper/pkg/kubernetes"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/yaml"

	"github.com/gardener/landscapercli/pkg/logger"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

type inputParametersOptions struct {
	installationPath string

	//input parameters that should be used for the import values
	inputParameters map[string]string
}

//NewSetInputParametersCommand sets input parameters from an installation to hardcoded values (as importDataMappings)
func NewSetInputParametersCommand(ctx context.Context) *cobra.Command {
	opts := &inputParametersOptions{}
	cmd := &cobra.Command{
		Use:     "set-input-parameters",
		Aliases: []string{"sip"},
		Short:   "set import parameters for an installation. Enquote string values in double quotation marks.",
		Example: "landscapercli installation set-input-parameters <path-to-installation>.yaml importName1=\"string-value\" importName2=42",
		Args:    cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if err := opts.validateArguments(args); err != nil {
				cmd.PrintErr(err.Error())
				os.Exit(1)
			}

			if err := opts.run(ctx, logger.Log, cmd); err != nil {
				cmd.PrintErr(err.Error())
				os.Exit(1)
			}
		},
	}
	cmd.SetOut(os.Stdout)

	return cmd
}

func (o *inputParametersOptions) validateArguments(args []string) error {
	o.installationPath = args[0]

	o.inputParameters = make(map[string]string)
	for _, v := range args[1:] {
		keyValue := strings.SplitN(v, "=", 2)
		o.inputParameters[keyValue[0]] = keyValue[1]
	}
	return nil
}

func (o *inputParametersOptions) run(ctx context.Context, log logr.Logger, cmd *cobra.Command) error {
	installation := lsv1alpha1.Installation{}

	err := readInstallationFromFile(o, &installation)
	if err != nil {
		return err
	}

	replaceImportsWithInputParameters(&installation, o)

	marshaledYaml, err := yaml.Marshal(installation)
	if err != nil {
		return fmt.Errorf("cannot marshal yaml: %w\nDid you enquote the value if it is a string?", err)
	}
	cmd.Println(string(marshaledYaml))

	return nil
}

func replaceImportsWithInputParameters(installation *lsv1alpha1.Installation, o *inputParametersOptions) {
	validImportDataMappings := make(map[string]json.RawMessage)

	//find all imports.data that are specified in inputParameters
	for _, importData := range installation.Spec.Imports.Data {
		if inputParameter, ok := o.inputParameters[importData.Name]; ok {
			validImportDataMappings[importData.Name] = json.RawMessage(fmt.Sprintf(`%s`, inputParameter))
		}
	}

	//modify the installation
	for importName, importDataMappingValue := range validImportDataMappings {
		if installation.Spec.ImportDataMappings == nil {
			installation.Spec.ImportDataMappings = make(map[string]json.RawMessage)
		}
		//add to importDataMappings
		installation.Spec.ImportDataMappings[importName] = importDataMappingValue

		//remove from imports.data
		for i, importData := range installation.Spec.Imports.Data {
			if importData.Name == importName {
				installation.Spec.Imports.Data = append(installation.Spec.Imports.Data[:i], installation.Spec.Imports.Data[i+1:]...)
			}
		}
	}
}

func readInstallationFromFile(o *inputParametersOptions, installation *lsv1alpha1.Installation) error {
	installationFileData, err := ioutil.ReadFile(o.installationPath)
	if err != nil {
		return err
	}
	if _, _, err := serializer.NewCodecFactory(kubernetes.LandscaperScheme).UniversalDecoder().Decode(installationFileData, nil, installation); err != nil {
		return err
	}
	return nil
}
