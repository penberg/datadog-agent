// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2020 Datadog, Inc.

package app

import (
	"fmt"

	"github.com/DataDog/datadog-agent/cmd/agent/common"
	"github.com/DataDog/datadog-agent/pkg/api/util"
	"github.com/DataDog/datadog-agent/pkg/config"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func init() {
	AgentCmd.AddCommand(troubleshootLogsCmd)
	// statusCmd.Flags().BoolVarP(&jsonStatus, "json", "j", false, "print out raw json")
	// statusCmd.Flags().BoolVarP(&prettyPrintJSON, "pretty-json", "p", false, "pretty print JSON")
	// statusCmd.Flags().StringVarP(&statusFilePath, "file", "o", "", "Output the status command to a file")
	// statusCmd.AddCommand(componentCmd)
	// componentCmd.Flags().BoolVarP(&prettyPrintJSON, "pretty-json", "p", false, "pretty print JSON")
	// componentCmd.Flags().StringVarP(&statusFilePath, "file", "o", "", "Output the status command to a file")
}

var troubleshootLogsCmd = &cobra.Command{
	Use:   "stream-logs",
	Short: "stream the logs being ",
	Long:  `Stream the actively ingested logs of a running agent`,
	RunE: func(cmd *cobra.Command, args []string) error {

		if flagNoColor {
			color.NoColor = true
		}

		err := common.SetupConfigWithoutSecrets(confFilePath, "")
		if err != nil {
			return fmt.Errorf("unable to set up global agent configuration: %v", err)
		}

		err = config.SetupLogger(loggerName, config.GetEnvDefault("DD_LOG_LEVEL", "off"), "", "", false, true, false)
		if err != nil {
			fmt.Printf("Cannot setup logger, exiting: %v\n", err)
			return err
		}

		return connectAndStream()
	},
}

func connectAndStream() error {
	ipcAddress, err := config.GetIPCAddress()
	if err != nil {
		return err
	}

	urlstr := fmt.Sprintf("https://%v:%v/agent/streamLogs", ipcAddress, config.Datadog.GetInt("cmd_port"))
	err = streamRequest(urlstr, func(chunk []byte) {
		fmt.Print(string(chunk))
	})

	if err != nil {
		return err
	}

	return nil
}

func streamRequest(url string, onChunk func([]byte)) error {
	var e error
	c := util.GetClient(false) // FIX: get certificates right then make this true

	// Set session token
	e = util.SetAuthToken()
	if e != nil {
		return e
	}

	e = util.DoGetChunked(c, url, onChunk)
	if e != nil {
		fmt.Printf("Could not reach agent: %v \nMake sure the agent is running before requesting the logs and contact support if you continue having issues. \n", e)
	}
	return e
}
