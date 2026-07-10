package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// UsageResult описывает одно место использования реквизита в модуле.
type UsageResult struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	Snippet  string `json:"snippet"`
}

// MetadataSearchResult описывает один найденный объект/реквизит метаданных.
type MetadataSearchResult struct {
	MetaType      string        `json:"meta_type"`
	ObjectName    string        `json:"object_name"`
	ObjectSynonym string        `json:"object_synonym"`
	ItemType      string        `json:"item_type"`
	ItemName      string        `json:"item_name"`
	ItemSynonym   string        `json:"item_synonym"`
	Usage         []UsageResult `json:"usage,omitempty"`
}

// FindMetadataBySynonymArgs аргументы инструмента find_metadata_by_synonym.
type FindMetadataBySynonymArgs struct {
	Synonym      string `json:"synonym"`
	MetaType     string `json:"meta_type,omitempty"`
	Language     string `json:"language,omitempty"`
	IncludeUsage bool   `json:"include_usage,omitempty"`
	MaxResults   int    `json:"max_results,omitempty"`
}

const defaultMaxMetadataResults = 20

// FindMetadataBySynonym ищет объект метаданных или реквизит по синониму в XML-исходниках конфигурации.
// Возвращает JSON-строку с массивом результатов.
func FindMetadataBySynonym(codeIndexPath string, argsJSON string) (string, error) {
	var args FindMetadataBySynonymArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("failed to parse args: %w", err)
	}
	if strings.TrimSpace(args.Synonym) == "" {
		return "", fmt.Errorf("synonym is required")
	}
	if args.Language == "" {
		args.Language = "en"
	}
	if args.MaxResults <= 0 {
		args.MaxResults = defaultMaxMetadataResults
	}

	searchRoot := codeIndexPath
	if args.MetaType != "" {
		switch strings.ToLower(args.MetaType) {
		case "catalog", "catalogs", "справочник", "справочники":
			searchRoot = filepath.Join(searchRoot, "Catalogs")
		case "document", "documents", "документ", "документы":
			searchRoot = filepath.Join(searchRoot, "Documents")
		case "informationregister", "informationregisters", "регистрсведений", "регистрысведений":
			searchRoot = filepath.Join(searchRoot, "InformationRegisters")
		case "accumulationregister", "accumulationregisters", "регистрнакопления", "регистрынакопления":
			searchRoot = filepath.Join(searchRoot, "AccumulationRegisters")
		}
	}

	pattern := regexp.QuoteMeta(args.Synonym)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("failed to compile regex: %w", err)
	}

	results := make([]MetadataSearchResult, 0)
	seen := make(map[string]bool)

	err = filepath.Walk(searchRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".xml") {
			return nil
		}
		fileResults, err := searchSynonymInXML(path, args.Synonym, re)
		if err != nil {
			log.Printf("[find_metadata_by_synonym] error parsing %s: %v", path, err)
			return nil
		}
		for _, r := range fileResults {
			key := r.MetaType + "." + r.ObjectName + "." + r.ItemType + "." + r.ItemName
			if seen[key] {
				continue
			}
			seen[key] = true
			results = append(results, r)
			if len(results) >= args.MaxResults {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to walk metadata files: %w", err)
	}

	// Поиск использования в модулях выполняем только по явному запросу,
	// чтобы не раздувать контекст сотнями килобайт текста модулей.
	if args.IncludeUsage {
		for i := range results {
			results[i].Usage = findUsageInModules(codeIndexPath, results[i].ItemName, 10)
		}
	}

	out, err := json.Marshal(map[string]interface{}{
		"results": results,
		"count":   len(results),
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal results: %w", err)
	}
	return string(out), nil
}

// findUsageInModules ищет использование внутреннего имени реквизита в модулях .bsl.
// Возвращает не более maxResults первых вхождений.
func findUsageInModules(codeIndexPath, itemName string, maxResults int) []UsageResult {
	if itemName == "" {
		return nil
	}
	pattern := regexp.QuoteMeta(itemName)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}

	usage := make([]UsageResult, 0, maxResults)
	searchDirs := []string{
		filepath.Join(codeIndexPath, "Catalogs"),
		filepath.Join(codeIndexPath, "Documents"),
		filepath.Join(codeIndexPath, "DataProcessors"),
		filepath.Join(codeIndexPath, "CommonModules"),
	}

	for _, dir := range searchDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".bsl") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			lines := strings.Split(string(data), "\n")
			for lineIdx, line := range lines {
				if re.MatchString(line) {
					snippet := strings.TrimSpace(line)
					if len(snippet) > 200 {
						snippet = snippet[:200]
					}
					relPath, _ := filepath.Rel(codeIndexPath, path)
					usage = append(usage, UsageResult{
						FilePath: relPath,
						Line:     lineIdx + 1,
						Snippet:  snippet,
					})
					if len(usage) >= maxResults {
						return filepath.SkipDir
					}
				}
			}
			return nil
		})
		if len(usage) >= maxResults {
			break
		}
	}
	return usage
}

var (
	reNameTag      = regexp.MustCompile(`<Name>([^<]+)</Name>`)
	reContentTag   = regexp.MustCompile(`<v8:content>([^<]+)</v8:content>`)
	reOpenTag      = regexp.MustCompile(`<([A-Za-z][A-Za-z0-9]*)\b`)
	reCloseTag     = regexp.MustCompile(`</([A-Za-z][A-Za-z0-9]*)>`)
	reObjectHeader = regexp.MustCompile(`(?s)<Properties>\s*<Name>([^<]+)</Name>.*?<Synonym>(.*?)</Synonym>`)
)

// searchSynonymInXML ищет синоним в одном XML-файле методом текстового анализа.
func searchSynonymInXML(path, synonym string, re *regexp.Regexp) ([]MetadataSearchResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	if !re.Match(data) {
		return nil, nil
	}

	text := string(data)
	metaType := guessMetaTypeFromPath(path)
	objectName, objectSynonym := extractObjectHeader(text)

	results := make([]MetadataSearchResult, 0)
	seen := make(map[string]bool)

	// Находим все вхождения синонима.
	for _, loc := range re.FindAllStringIndex(text, -1) {
		pos := loc[0]
		itemType, itemName, itemSynonym := extractItemAtPosition(text, pos)
		if itemName == "" {
			continue
		}
		key := itemType + "." + itemName
		if seen[key] {
			continue
		}
		seen[key] = true
		results = append(results, MetadataSearchResult{
			MetaType:      metaType,
			ObjectName:    objectName,
			ObjectSynonym: objectSynonym,
			ItemType:      itemType,
			ItemName:      itemName,
			ItemSynonym:   itemSynonym,
		})
	}

	// Если синоним совпадает с синонимом самого объекта — добавляем и объект.
	if strings.EqualFold(objectSynonym, synonym) || strings.Contains(objectSynonym, synonym) {
		key := metaType + "." + objectName
		if !seen[key] {
			results = append(results, MetadataSearchResult{
				MetaType:      metaType,
				ObjectName:    objectName,
				ObjectSynonym: objectSynonym,
				ItemType:      metaType,
				ItemName:      objectName,
				ItemSynonym:   objectSynonym,
			})
		}
	}

	return results, nil
}

// extractObjectHeader извлекает имя и синоним объекта из корневых Properties.
func extractObjectHeader(text string) (name, synonym string) {
	m := reObjectHeader.FindStringSubmatch(text)
	if len(m) < 2 {
		return "", ""
	}
	name = strings.TrimSpace(m[1])
	if len(m) >= 3 {
		synonym = firstContent(m[2])
	}
	return
}

// firstContent возвращает первый непустой <v8:content> из фрагмента.
func firstContent(fragment string) string {
	m := reContentTag.FindStringSubmatch(fragment)
	if len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// extractItemAtPosition определяет контейнер (Attribute/Dimension/...) и его Name
// для позиции вхождения синонима.
func extractItemAtPosition(text string, pos int) (itemType, itemName, itemSynonym string) {
	// Ищем ближайший открывающий тег контейнера слева от pos.
	stack := make([]tagInfo, 0)
	scanPos := 0
	for scanPos < pos {
		// Сначала ищем открывающий тег.
		openLoc := reOpenTag.FindStringIndex(text[scanPos:])
		if openLoc == nil {
			break
		}
		openStart := scanPos + openLoc[0]
		openEnd := scanPos + openLoc[1]
		tagName := reOpenTag.FindStringSubmatch(text[scanPos:])[1]

		// Ищем закрывающий тег того же имени после открывающего.
		closeRe := regexp.MustCompile(`</` + tagName + `>`)
		closeLoc := closeRe.FindStringIndex(text[openEnd:])
		if closeLoc == nil {
			scanPos = openEnd
			continue
		}
		closeEnd := openEnd + closeLoc[1]

		if isItemContainerName(tagName) && openStart < pos && pos < closeEnd {
			// Это наш контейнер. Извлекаем Name и content.
			fragment := text[openStart:closeEnd]
			nameMatch := reNameTag.FindStringSubmatch(fragment)
			if len(nameMatch) >= 2 {
				itemName = strings.TrimSpace(nameMatch[1])
			}
			contentMatch := reContentTag.FindStringSubmatch(fragment)
			if len(contentMatch) >= 2 {
				itemSynonym = strings.TrimSpace(contentMatch[1])
			}
			itemType = tagName
			return
		}

		scanPos = openEnd
		_ = stack
	}
	return
}

type tagInfo struct {
	Name  string
	Start int
	End   int
}

// isItemContainerName возвращает true, если тег — контейнер реквизита/ТЧ/команды/шаблона.
func isItemContainerName(name string) bool {
	switch name {
	case "Attribute", "Dimension", "Resource", "TabularSection", "Command", "Template":
		return true
	}
	return false
}

// guessMetaTypeFromPath определяет тип метаданных по пути файла.
func guessMetaTypeFromPath(path string) string {
	lower := strings.ToLower(filepath.ToSlash(path))
	switch {
	case strings.Contains(lower, "/catalogs/"):
		return "Catalog"
	case strings.Contains(lower, "/documents/"):
		return "Document"
	case strings.Contains(lower, "/informationregisters/"):
		return "InformationRegister"
	case strings.Contains(lower, "/accumulationregisters/"):
		return "AccumulationRegister"
	case strings.Contains(lower, "/dataprocessors/"):
		return "DataProcessor"
	case strings.Contains(lower, "/reports/"):
		return "Report"
	case strings.Contains(lower, "/charts_of_accounts/"):
		return "ChartOfAccounts"
	case strings.Contains(lower, "/charts_of_characteristic_types/"):
		return "ChartOfCharacteristicTypes"
	default:
		return "Unknown"
	}
}
