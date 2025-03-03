package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/canonical/lxd/shared"
)

var (
	globalLxdDocRegex   = regexp.MustCompile(`(?m)lxddoc:generate\((.*)\)([\S\s]+)\s+---\n([\S\s]+)`)
	lxdDocMetadataRegex = regexp.MustCompile(`(?m)([^,\s]+)=([^,\s]+)`)
	lxdDocDataRegex     = regexp.MustCompile(`(?m)([\S]+):[\s]+([\S \"\']+)`)
)

var mdKeys []string = []string{"group", "key"}

type doc struct {
	Configs map[string][]any
}

func detectType(s string) any {
	i, err := strconv.Atoi(s)
	if err == nil {
		return i
	}

	b, err := strconv.ParseBool(s)
	if err == nil {
		return b
	}

	f, err := strconv.ParseFloat(s, 64)
	if err == nil {
		return f
	}

	t, err := time.Parse(time.RFC3339, s)
	if err == nil {
		return t
	}

	// special characters handling
	if s == "-" {
		return ""
	}

	// If all conversions fail, it's a string
	return s
}

func parse(path string, outputYAMLPath string, excludedPaths []string) (*doc, error) {
	yamlDoc := &doc{}
	docKeys := make(map[string]struct{}, 0)
	projectEntries := make(map[string][]any)
	err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip excluded paths
		if shared.StringInSlice(path, excludedPaths) {
			if info.IsDir() {
				logger.Printf("Skipping excluded directory: %v", path)
				return filepath.SkipDir
			}

			logger.Printf("Skipping excluded file: %v", path)
			return nil
		}

		// Only process go files
		if !info.IsDir() && filepath.Ext(path) != ".go" {
			logger.Printf("Skipping non-golang file: %v", path)
			return nil
		}

		// Continue walking if directory
		if info.IsDir() {
			return nil
		}

		// Parse file and create the AST
		var fset = token.NewFileSet()
		var f *ast.File
		f, err = parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}

		fileEntries := make([]map[string]any, 0)

		// Loop in comment groups
		for _, cg := range f.Comments {
			s := cg.Text()
			entry := make(map[string]any)
			for _, match := range globalLxdDocRegex.FindAllStringSubmatch(s, -1) {
				// check that the match contains the expected number of groups
				if len(match) != 4 {
					continue
				}

				logger.Printf("Found lxddoc at %s", fset.Position(cg.Pos()).String())
				metadata := match[1]
				longdesc := match[2]
				data := match[3]
				// process metadata
				metadataMap := make(map[string]string)
				var groupKey string
				var simpleKey string
				for _, mdKVMatch := range lxdDocMetadataRegex.FindAllStringSubmatch(metadata, -1) {
					if len(mdKVMatch) != len(mdKeys)+1 {
						continue
					}

					mdKey := mdKVMatch[1]
					mdValue := mdKVMatch[2]
					// check that the metadata key is among the expected ones
					if !shared.StringInSlice(mdKey, mdKeys) {
						continue
					}

					if mdKey == "group" {
						groupKey = mdValue
					}

					if mdKey == "key" {
						simpleKey = mdValue
					}

					metadataMap[mdKey] = mdValue
				}

				// Check that this metadata is not already present
				mdKeyHash := fmt.Sprintf("%s/%s", groupKey, simpleKey)
				_, ok := docKeys[mdKeyHash]
				if ok {
					return fmt.Errorf("Duplicate key '%s' found at %s", mdKeyHash, fset.Position(cg.Pos()).String())
				}

				docKeys[mdKeyHash] = struct{}{}

				configKeyEntry := make(map[string]any)
				configKeyEntry[metadataMap["key"]] = make(map[string]any)
				configKeyEntry[metadataMap["key"]].(map[string]any)["longdesc"] = strings.TrimLeft(longdesc, "\n\t\v\f\r")
				entry[metadataMap["group"]] = configKeyEntry

				// process data
				for _, dataKVMatch := range lxdDocDataRegex.FindAllStringSubmatch(data, -1) {
					if len(dataKVMatch) != 3 {
						continue
					}

					entry[metadataMap["group"]].(map[string]any)[metadataMap["key"]].(map[string]any)[dataKVMatch[1]] = detectType(dataKVMatch[2])
				}
			}

			if len(entry) > 0 {
				fileEntries = append(fileEntries, entry)
			}
		}

		// Update projectEntries
		for _, entry := range fileEntries {
			for k, v := range entry {
				_, ok := projectEntries[k]
				if !ok {
					projectEntries[k] = []any{v}
				} else {
					projectEntries[k] = append(projectEntries[k], v)
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	yamlDoc.Configs = projectEntries
	data, err := yaml.Marshal(yamlDoc)
	if err != nil {
		return nil, fmt.Errorf("Error while marshaling project documentation: %v", err)
	}

	if outputYAMLPath != "" {
		buf := bytes.NewBufferString("# Code generated by lxd-doc; DO NOT EDIT.\n\n")
		_, err = buf.Write(data)
		if err != nil {
			return nil, fmt.Errorf("Error while writing the YAML project documentation: %v", err)
		}

		err := ioutil.WriteFile(outputYAMLPath, buf.Bytes(), 0644)
		if err != nil {
			return nil, fmt.Errorf("Error while writing the YAML project documentation: %v", err)
		}
	}

	return yamlDoc, nil
}

func writeDocFile(path string, yaml *doc) error {
	countMaxBackTicks := func(s string) int {
		count, curr_count := 0, 0
		n := len(s)
		for i := 0; i < n; i++ {
			if s[i] == '`' {
				curr_count++
				continue
			}

			if curr_count > count {
				count = curr_count
			}

			curr_count = 0
		}

		return count
	}

	specialChars := []string{"", "*", "_", "#", "+", "-", ".", "!", "no", "yes"}

	// create a string buffer
	buffer := bytes.NewBufferString("// Code generated by lxd-doc; DO NOT EDIT.\n\n")
	for groupKey, groupEntries := range yaml.Configs {
		buffer.WriteString(fmt.Sprintf("<!-- config group %s start -->\n", groupKey))
		for _, configEntry := range groupEntries {
			for configKey, configEntryContent := range configEntry.(map[string]any) {
				kvBuffer := bytes.NewBufferString("")
				var backticksCount int
				var longDescContent string
				for configEntryContentKey, configEntryContentValue := range configEntryContent.(map[string]any) {
					if configEntryContentKey == "longdesc" {
						backticksCount = countMaxBackTicks(configEntryContentValue.(string))
						longDescContent = configEntryContentValue.(string)
						continue
					}

					configEntryContentValueStr, ok := configEntryContentValue.(string)
					if ok {
						if (strings.HasSuffix(configEntryContentValueStr, "`") && strings.HasPrefix(configEntryContentValueStr, "`")) || shared.StringInSlice(configEntryContentValueStr, specialChars) {
							configEntryContentValueStr = fmt.Sprintf("\"%s\"", configEntryContentValueStr)
						}
					} else {
						switch configEntryContentTyped := configEntryContentValue.(type) {
						case int, float64, bool:
							configEntryContentValueStr = fmt.Sprint(configEntryContentTyped)
						case time.Time:
							configEntryContentValueStr = fmt.Sprint(configEntryContentTyped.Format(time.RFC3339))
						}
					}

					kvBuffer.WriteString(
						fmt.Sprintf(
							":%s: %s\n",
							configEntryContentKey,
							configEntryContentValueStr,
						),
					)
				}

				if backticksCount < 3 {
					buffer.WriteString(
						fmt.Sprintf("```{config:option} %s %s\n%s%s\n```\n\n",
							configKey,
							groupKey,
							kvBuffer.String(),
							strings.TrimLeft(longDescContent, "\n"),
						))
				} else {
					configQuotes := strings.Repeat("`", backticksCount+1)
					buffer.WriteString(
						fmt.Sprintf("%s{config:option} %s %s\n%s%s\n%s\n\n",
							configQuotes,
							configKey,
							groupKey,
							kvBuffer.String(),
							strings.TrimLeft(longDescContent, "\n"),
							configQuotes,
						))
				}
			}
		}

		buffer.WriteString(fmt.Sprintf("<!-- config group %s end -->\n", groupKey))
	}

	err := ioutil.WriteFile(path, buffer.Bytes(), 0644)
	if err != nil {
		return fmt.Errorf("Error while writing the Markdown project documentation: %v", err)
	}

	return nil
}
