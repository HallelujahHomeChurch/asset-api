package assets

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const personalPptxEntryLimit = 32 << 20
const personalPptxTotalLimit = 256 << 20
const personalPptxMediaLimit = 192 << 20
const personalPptxPresentationNS = "http://schemas.openxmlformats.org/presentationml/2006/main"
const personalPptxRelationshipNS = "http://schemas.openxmlformats.org/package/2006/relationships"
const personalPptxContentTypeNS = "http://schemas.openxmlformats.org/package/2006/content-types"
const personalPptxOfficeRelationship = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument"

var personalPptxDTD = regexp.MustCompile(`(?i)<!\s*(?:DOCTYPE|ENTITY)\b`)
var personalPptxSlidePart = regexp.MustCompile(`^ppt/slides/[^/]+\.xml$`)

type personalDeckSource struct {
	Version int    `json:"version"`
	Base64  string `json:"base64"`
	SHA256  string `json:"sha256"`
}

type personalDeckSourceNode struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

type personalDeckSourceSlide struct {
	ID           string                         `json:"id"`
	ThemeID      string                         `json:"themeId"`
	ElementOrder []string                       `json:"elementOrder"`
	Elements     map[string]personalDeckElement `json:"elements"`
	SourcePart   json.RawMessage                `json:"pptxSourcePart"`
}

type personalZipMember struct {
	name string
	size uint32
	crc  uint32
}

func personalPptxPath(name string) bool {
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, `\:`) {
		return false
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return false
		}
	}
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func personalPptxExtra(data []byte) bool {
	for len(data) > 0 {
		if len(data) < 4 || binary.LittleEndian.Uint16(data) == 1 {
			return false
		}
		length := 4 + int(binary.LittleEndian.Uint16(data[2:]))
		if length > len(data) {
			return false
		}
		data = data[length:]
	}
	return true
}

// Check the raw directory before archive/zip can interpret aliases or ZIP64 fields.
func personalPptxDirectory(data []byte) ([]personalZipMember, error) {
	if len(data) < 22 || int64(len(data)) > PersonalMaxFileSize {
		return nil, ErrInvalidUpload
	}
	end := len(data) - 22
	minimum := max(0, end-65535)
	for end >= minimum {
		if binary.LittleEndian.Uint32(data[end:]) == 0x06054b50 && end+22+int(binary.LittleEndian.Uint16(data[end+20:])) == len(data) {
			break
		}
		end--
	}
	if end < minimum {
		return nil, ErrInvalidUpload
	}
	u16 := func(offset int) uint16 { return binary.LittleEndian.Uint16(data[offset:]) }
	u32 := func(offset int) uint32 { return binary.LittleEndian.Uint32(data[offset:]) }
	count := int(u16(end + 10))
	directory := int(u32(end + 16))
	if u16(end+4) != 0 || u16(end+6) != 0 || count == 0 || count > 4000 || int(u16(end+8)) != count || directory+int(u32(end+12)) != end {
		return nil, ErrInvalidUpload
	}
	members := make([]personalZipMember, 0, count)
	names := map[string]bool{}
	type span struct{ start, end int }
	spans := make([]span, 0, count)
	var total, media uint64
	offset := directory
	for range count {
		if offset+46 > end || u32(offset) != 0x02014b50 {
			return nil, ErrInvalidUpload
		}
		flags, method := u16(offset+8), u16(offset+10)
		crc, compressed, size := u32(offset+16), u32(offset+20), u32(offset+24)
		nameSize, extraSize, commentSize := int(u16(offset+28)), int(u16(offset+30)), int(u16(offset+32))
		local := int(u32(offset + 42))
		if flags&1 != 0 || (method != 0 && method != 8) || u16(offset+34) != 0 || size == 0xffffffff || compressed == 0xffffffff || local == 0xffffffff || offset+46+nameSize+extraSize+commentSize > end {
			return nil, ErrInvalidUpload
		}
		name := string(data[offset+46 : offset+46+nameSize])
		if !personalPptxPath(name) || names[name] || !personalPptxExtra(data[offset+46+nameSize:offset+46+nameSize+extraSize]) {
			return nil, ErrInvalidUpload
		}
		names[name] = true
		for _, part := range strings.Split(strings.ToLower(name), "/") {
			if part == "vbaproject" || strings.HasPrefix(part, "vbaproject.") {
				return nil, ErrInvalidUpload
			}
		}
		total += uint64(size)
		if strings.HasPrefix(name, "ppt/media/") {
			media += uint64(size)
		}
		if size > personalPptxEntryLimit || total > personalPptxTotalLimit || media > personalPptxMediaLimit {
			return nil, ErrInvalidUpload
		}
		if local+30 > directory || u32(local) != 0x04034b50 {
			return nil, ErrInvalidUpload
		}
		localNameSize, localExtraSize := int(u16(local+26)), int(u16(local+28))
		start := local + 30 + localNameSize + localExtraSize
		if start+int(compressed) > directory || u16(local+6) != flags || u16(local+8) != method {
			return nil, ErrInvalidUpload
		}
		if string(data[local+30:local+30+localNameSize]) != name || !personalPptxExtra(data[local+30+localNameSize:start]) {
			return nil, ErrInvalidUpload
		}
		if flags&8 == 0 && (u32(local+14) != crc || u32(local+18) != compressed || u32(local+22) != size) {
			return nil, ErrInvalidUpload
		}
		for _, previous := range spans {
			if local < previous.end && start+int(compressed) > previous.start {
				return nil, ErrInvalidUpload
			}
		}
		spans = append(spans, span{local, start + int(compressed)})
		members = append(members, personalZipMember{name, size, crc})
		offset += 46 + nameSize + extraSize + commentSize
	}
	if offset != end {
		return nil, ErrInvalidUpload
	}
	return members, nil
}

type personalPptxRel struct{ id, kind, target, mode string }
type personalPptxXML struct {
	root                       xml.Name
	defaults, overrides, nodes map[string]string
	rels                       []personalPptxRel
}

func personalXMLAttr(element xml.StartElement, name string) string {
	for _, attribute := range element.Attr {
		if attribute.Name.Space == "" && attribute.Name.Local == name {
			return attribute.Value
		}
	}
	return ""
}

func personalPptxXMLRead(ctx context.Context, data []byte) (personalPptxXML, error) {
	result := personalPptxXML{defaults: map[string]string{}, overrides: map[string]string{}, nodes: map[string]string{}}
	if !utf8.Valid(data) || personalPptxDTD.Match(data) {
		return result, ErrInvalidUpload
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	decoder := xml.NewDecoder(&contextReader{ctx: ctx, reader: bytes.NewReader(data)})
	stack := []xml.Name{}
	roots := 0
	ids := map[string]bool{}
	relIDs := map[string]bool{}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, ErrInvalidUpload
		}
		switch element := token.(type) {
		case xml.StartElement:
			attributes := map[xml.Name]bool{}
			for _, a := range element.Attr {
				if attributes[a.Name] {
					return result, ErrInvalidUpload
				}
				attributes[a.Name] = true
			}
			stack = append(stack, element.Name)
			if len(stack) > 128 {
				return result, ErrInvalidUpload
			}
			if len(stack) == 1 {
				roots++
				result.root = element.Name
				if roots > 1 {
					return result, ErrInvalidUpload
				}
			}
			if len(stack) == 2 && result.root == (xml.Name{Space: personalPptxContentTypeNS, Local: "Types"}) {
				kind := personalXMLAttr(element, "ContentType")
				if kind == "" || element.Name.Space != personalPptxContentTypeNS || strings.Contains(strings.ToLower(kind), "macroenabled") || strings.Contains(strings.ToLower(kind), "vba") {
					return result, ErrInvalidUpload
				}
				if element.Name.Local == "Default" {
					extension := personalXMLAttr(element, "Extension")
					if extension == "" || result.defaults[extension] != "" {
						return result, ErrInvalidUpload
					}
					result.defaults[extension] = kind
				} else if element.Name.Local == "Override" {
					part := personalXMLAttr(element, "PartName")
					if !strings.HasPrefix(part, "/") || result.overrides[strings.TrimPrefix(part, "/")] != "" {
						return result, ErrInvalidUpload
					}
					result.overrides[strings.TrimPrefix(part, "/")] = kind
				} else {
					return result, ErrInvalidUpload
				}
			}
			if len(stack) == 2 && result.root == (xml.Name{Space: personalPptxRelationshipNS, Local: "Relationships"}) {
				rel := personalPptxRel{personalXMLAttr(element, "Id"), personalXMLAttr(element, "Type"), personalXMLAttr(element, "Target"), personalXMLAttr(element, "TargetMode")}
				if element.Name != (xml.Name{Space: personalPptxRelationshipNS, Local: "Relationship"}) || rel.id == "" || rel.kind == "" || rel.target == "" || relIDs[rel.id] {
					return result, ErrInvalidUpload
				}
				relIDs[rel.id] = true
				result.rels = append(result.rels, rel)
			}
			if result.root == (xml.Name{Space: personalPptxPresentationNS, Local: "sld"}) && element.Name == (xml.Name{Space: personalPptxPresentationNS, Local: "cNvPr"}) {
				id := personalXMLAttr(element, "id")
				if id == "" || ids[id] {
					return result, ErrInvalidUpload
				}
				ids[id] = true
				for index := len(stack) - 2; index >= 0; index-- {
					if stack[index].Space == personalPptxPresentationNS && (stack[index].Local == "sp" || stack[index].Local == "pic") {
						result.nodes[id] = stack[index].Local
						break
					}
				}
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.Directive:
			return result, ErrInvalidUpload
		case xml.CharData:
			if len(stack) == 0 && strings.TrimSpace(string(element)) != "" {
				return result, ErrInvalidUpload
			}
		}
	}
	if roots != 1 || len(stack) != 0 {
		return result, ErrInvalidUpload
	}
	return result, ctx.Err()
}

func personalPptxTarget(owner, target string) (string, error) {
	if target == "" || strings.Contains(target, `\`) {
		return "", ErrInvalidUpload
	}
	for _, r := range target {
		if r < 32 {
			return "", ErrInvalidUpload
		}
	}
	base, _ := url.Parse("https://pptx.invalid/" + owner)
	relative, err := url.Parse(target)
	if err != nil {
		return "", ErrInvalidUpload
	}
	resolved := base.ResolveReference(relative)
	name := strings.TrimPrefix(resolved.Path, "/")
	if resolved.Scheme != "https" || resolved.Host != "pptx.invalid" || resolved.RawQuery != "" || !personalPptxPath(name) {
		return "", ErrInvalidUpload
	}
	return name, nil
}

func personalPptxInspect(ctx context.Context, data []byte) (map[string]map[string]string, error) {
	members, err := personalPptxDirectory(data)
	if err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(archive.File) != len(members) {
		return nil, ErrInvalidUpload
	}
	names := map[string]bool{}
	documents := map[string]personalPptxXML{}
	for index, file := range archive.File {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		member := members[index]
		if file.Name != member.name || file.CRC32 != member.crc || file.UncompressedSize64 != uint64(member.size) {
			return nil, ErrInvalidUpload
		}
		names[file.Name] = true
		if strings.HasSuffix(file.Name, "/") {
			if member.size != 0 {
				return nil, ErrInvalidUpload
			}
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return nil, ErrInvalidUpload
		}
		content, readErr := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: reader}, int64(member.size)+1))
		closeErr := reader.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr != nil || closeErr != nil || len(content) != int(member.size) {
			return nil, ErrInvalidUpload
		}
		lower := strings.ToLower(file.Name)
		if strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".rels") {
			document, err := personalPptxXMLRead(ctx, content)
			if err != nil {
				return nil, err
			}
			documents[file.Name] = document
		}
	}
	types, ok := documents["[Content_Types].xml"]
	if !ok || types.root != (xml.Name{Space: personalPptxContentTypeNS, Local: "Types"}) {
		return nil, ErrInvalidUpload
	}
	for name := range types.overrides {
		if !names[name] {
			return nil, ErrInvalidUpload
		}
	}
	for name := range names {
		if name == "[Content_Types].xml" || strings.HasSuffix(name, "/") {
			continue
		}
		pieces := strings.Split(name, ".")
		if types.overrides[name] == "" && types.defaults[pieces[len(pieces)-1]] == "" {
			return nil, ErrInvalidUpload
		}
	}
	presentation := ""
	for name, document := range documents {
		if !strings.HasSuffix(name, ".rels") {
			continue
		}
		if document.root != (xml.Name{Space: personalPptxRelationshipNS, Local: "Relationships"}) {
			return nil, ErrInvalidUpload
		}
		owner := ""
		if name != "_rels/.rels" {
			owner = strings.TrimSuffix(strings.Replace(name, "/_rels/", "/", 1), ".rels")
			if strings.HasPrefix(owner, "_rels/") {
				owner = strings.TrimPrefix(owner, "_rels/")
			}
			if owner == name || !names[owner] {
				return nil, ErrInvalidUpload
			}
		}
		for _, rel := range document.rels {
			if rel.mode == "External" {
				continue
			}
			if rel.mode != "" && rel.mode != "Internal" {
				return nil, ErrInvalidUpload
			}
			target, err := personalPptxTarget(owner, rel.target)
			if err != nil || !names[target] {
				return nil, ErrInvalidUpload
			}
			if name == "_rels/.rels" && rel.kind == personalPptxOfficeRelationship {
				if presentation != "" {
					return nil, ErrInvalidUpload
				}
				presentation = target
			}
		}
	}
	root, ok := documents[presentation]
	if !ok || root.root != (xml.Name{Space: personalPptxPresentationNS, Local: "presentation"}) || types.overrides[presentation] != "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml" {
		return nil, ErrInvalidUpload
	}
	slides := map[string]map[string]string{}
	for name, document := range documents {
		if personalPptxSlidePart.MatchString(name) {
			if document.root != (xml.Name{Space: personalPptxPresentationNS, Local: "sld"}) {
				return nil, ErrInvalidUpload
			}
			slides[name] = document.nodes
		}
	}
	return slides, ctx.Err()
}

func validatePersonalDeckSource(ctx context.Context, source *personalDeckSource, slides map[string]personalDeckSourceSlide) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if source == nil {
		for _, slide := range slides {
			if len(slide.SourcePart) > 0 {
				return ErrInvalidUpload
			}
			for _, e := range slide.Elements {
				if len(e.SourceNode) > 0 {
					return ErrInvalidUpload
				}
			}
		}
		return nil
	}
	if source.Version != 1 || len(source.Base64) == 0 || len(source.Base64) > base64.StdEncoding.EncodedLen(int(PersonalMaxFileSize)) || len(source.SHA256) != 64 || strings.ToLower(source.SHA256) != source.SHA256 {
		return ErrInvalidUpload
	}
	data, err := base64.StdEncoding.Strict().DecodeString(source.Base64)
	if err != nil || int64(len(data)) > PersonalMaxFileSize || base64.StdEncoding.EncodeToString(data) != source.Base64 {
		return ErrInvalidUpload
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != source.SHA256 {
		return ErrInvalidUpload
	}
	nodes, err := personalPptxInspect(ctx, data)
	if err != nil {
		return err
	}
	claims := map[string]bool{}
	for _, slide := range slides {
		part := ""
		if len(slide.SourcePart) > 0 {
			if err := json.Unmarshal(slide.SourcePart, &part); err != nil || part == "" || !personalPptxSlidePart.MatchString(part) || nodes[part] == nil {
				return ErrInvalidUpload
			}
		}
		for _, element := range slide.Elements {
			if len(element.SourceNode) == 0 {
				continue
			}
			var reference *personalDeckSourceNode
			if err := json.Unmarshal(element.SourceNode, &reference); err != nil || reference == nil || reference.ID == "" || len(reference.ID) > 512 {
				return ErrInvalidUpload
			}
			expected := "sp"
			switch reference.Role {
			case "image":
				if element.Type != "image" {
					return ErrInvalidUpload
				}
				expected = "pic"
			case "text":
				if element.Type != "text" {
					return ErrInvalidUpload
				}
			case "geometry":
				if element.Type != "shape" && element.Type != "line" {
					return ErrInvalidUpload
				}
			default:
				return ErrInvalidUpload
			}
			if part == "" || nodes[part][reference.ID] != expected {
				return ErrInvalidUpload
			}
			claim := part + "\x00" + reference.ID + "\x00" + reference.Role
			if claims[claim] {
				return ErrInvalidUpload
			}
			claims[claim] = true
		}
	}
	return ctx.Err()
}
