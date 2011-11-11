// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package html

import (
	"io"
	"strings"
)

// A parser implements the HTML5 parsing algorithm:
// http://www.whatwg.org/specs/web-apps/current-work/multipage/tokenization.html#tree-construction
type parser struct {
	// tokenizer provides the tokens for the parser.
	tokenizer *Tokenizer
	// tok is the most recently read token.
	tok Token
	// Self-closing tags like <hr/> are re-interpreted as a two-token sequence:
	// <hr> followed by </hr>. hasSelfClosingToken is true if we have just read
	// the synthetic start tag and the next one due is the matching end tag.
	hasSelfClosingToken bool
	// doc is the document root element.
	doc *Node
	// The stack of open elements (section 11.2.3.2) and active formatting
	// elements (section 11.2.3.3).
	oe, afe nodeStack
	// Element pointers (section 11.2.3.4).
	head, form *Node
	// Other parsing state flags (section 11.2.3.5).
	scripting, framesetOK bool
	// originalIM is the insertion mode to go back to after completing a text
	// or inTableText insertion mode.
	originalIM insertionMode
	// fosterParenting is whether new elements should be inserted according to
	// the foster parenting rules (section 11.2.5.3).
	fosterParenting bool
}

func (p *parser) top() *Node {
	if n := p.oe.top(); n != nil {
		return n
	}
	return p.doc
}

// stopTags for use in popUntil. These come from section 11.2.3.2.
var (
	defaultScopeStopTags  = []string{"applet", "caption", "html", "table", "td", "th", "marquee", "object"}
	listItemScopeStopTags = []string{"applet", "caption", "html", "table", "td", "th", "marquee", "object", "ol", "ul"}
	buttonScopeStopTags   = []string{"applet", "caption", "html", "table", "td", "th", "marquee", "object", "button"}
	tableScopeStopTags    = []string{"html", "table"}
)

// stopTags for use in clearStackToContext.
var (
	tableRowContextStopTags = []string{"tr", "html"}
)

// popUntil pops the stack of open elements at the highest element whose tag
// is in matchTags, provided there is no higher element in stopTags. It returns
// whether or not there was such an element. If there was not, popUntil leaves
// the stack unchanged.
//
// For example, if the stack was:
// ["html", "body", "font", "table", "b", "i", "u"]
// then popUntil([]string{"html, "table"}, "font") would return false, but
// popUntil([]string{"html, "table"}, "i") would return true and the resultant
// stack would be:
// ["html", "body", "font", "table", "b"]
//
// If an element's tag is in both stopTags and matchTags, then the stack will
// be popped and the function returns true (provided, of course, there was no
// higher element in the stack that was also in stopTags). For example,
// popUntil([]string{"html, "table"}, "table") would return true and leave:
// ["html", "body", "font"]
func (p *parser) popUntil(stopTags []string, matchTags ...string) bool {
	if i := p.indexOfElementInScope(stopTags, matchTags...); i != -1 {
		p.oe = p.oe[:i]
		return true
	}
	return false
}

// indexOfElementInScope returns the index in p.oe of the highest element
// whose tag is in matchTags that is in scope according to stopTags.
// If no matching element is in scope, it returns -1.
func (p *parser) indexOfElementInScope(stopTags []string, matchTags ...string) int {
	for i := len(p.oe) - 1; i >= 0; i-- {
		tag := p.oe[i].Data
		for _, t := range matchTags {
			if t == tag {
				return i
			}
		}
		for _, t := range stopTags {
			if t == tag {
				return -1
			}
		}
	}
	return -1
}

// elementInScope is like popUntil, except that it doesn't modify the stack of
// open elements.
func (p *parser) elementInScope(stopTags []string, matchTags ...string) bool {
	return p.indexOfElementInScope(stopTags, matchTags...) != -1
}

// addChild adds a child node n to the top element, and pushes n onto the stack
// of open elements if it is an element node.
func (p *parser) addChild(n *Node) {
	if p.fosterParenting {
		p.fosterParent(n)
	} else {
		p.top().Add(n)
	}

	if n.Type == ElementNode {
		p.oe = append(p.oe, n)
	}
}

// fosterParent adds a child node according to the foster parenting rules.
// Section 11.2.5.3, "foster parenting".
func (p *parser) fosterParent(n *Node) {
	p.fosterParenting = false
	var table, parent *Node
	var i int
	for i = len(p.oe) - 1; i >= 0; i-- {
		if p.oe[i].Data == "table" {
			table = p.oe[i]
			break
		}
	}

	if table == nil {
		// The foster parent is the html element.
		parent = p.oe[0]
	} else {
		parent = table.Parent
	}
	if parent == nil {
		parent = p.oe[i-1]
	}

	var child *Node
	for i, child = range parent.Child {
		if child == table {
			break
		}
	}

	if i > 0 && parent.Child[i-1].Type == TextNode && n.Type == TextNode {
		parent.Child[i-1].Data += n.Data
		return
	}

	if i == len(parent.Child) {
		parent.Add(n)
	} else {
		// Insert n into parent.Child at index i.
		parent.Child = append(parent.Child[:i+1], parent.Child[i:]...)
		parent.Child[i] = n
		n.Parent = parent
	}
}

// addText adds text to the preceding node if it is a text node, or else it
// calls addChild with a new text node.
func (p *parser) addText(text string) {
	// TODO: distinguish whitespace text from others.
	t := p.top()
	if i := len(t.Child); i > 0 && t.Child[i-1].Type == TextNode {
		t.Child[i-1].Data += text
		return
	}
	p.addChild(&Node{
		Type: TextNode,
		Data: text,
	})
}

// addElement calls addChild with an element node.
func (p *parser) addElement(tag string, attr []Attribute) {
	p.addChild(&Node{
		Type: ElementNode,
		Data: tag,
		Attr: attr,
	})
}

// Section 11.2.3.3.
func (p *parser) addFormattingElement(tag string, attr []Attribute) {
	p.addElement(tag, attr)
	p.afe = append(p.afe, p.top())
	// TODO.
}

// Section 11.2.3.3.
func (p *parser) clearActiveFormattingElements() {
	for {
		n := p.afe.pop()
		if len(p.afe) == 0 || n.Type == scopeMarkerNode {
			return
		}
	}
}

// Section 11.2.3.3.
func (p *parser) reconstructActiveFormattingElements() {
	n := p.afe.top()
	if n == nil {
		return
	}
	if n.Type == scopeMarkerNode || p.oe.index(n) != -1 {
		return
	}
	i := len(p.afe) - 1
	for n.Type != scopeMarkerNode && p.oe.index(n) == -1 {
		if i == 0 {
			i = -1
			break
		}
		i--
		n = p.afe[i]
	}
	for {
		i++
		clone := p.afe[i].clone()
		p.addChild(clone)
		p.afe[i] = clone
		if i == len(p.afe)-1 {
			break
		}
	}
}

// read reads the next token. This is usually from the tokenizer, but it may
// be the synthesized end tag implied by a self-closing tag.
func (p *parser) read() error {
	if p.hasSelfClosingToken {
		p.hasSelfClosingToken = false
		p.tok.Type = EndTagToken
		p.tok.Attr = nil
		return nil
	}
	p.tokenizer.Next()
	p.tok = p.tokenizer.Token()
	switch p.tok.Type {
	case ErrorToken:
		return p.tokenizer.Err()
	case SelfClosingTagToken:
		p.hasSelfClosingToken = true
		p.tok.Type = StartTagToken
	}
	return nil
}

// Section 11.2.4.
func (p *parser) acknowledgeSelfClosingTag() {
	p.hasSelfClosingToken = false
}

// An insertion mode (section 11.2.3.1) is the state transition function from
// a particular state in the HTML5 parser's state machine. It updates the
// parser's fields depending on parser.token (where ErrorToken means EOF). In
// addition to returning the next insertionMode state, it also returns whether
// the token was consumed.
type insertionMode func(*parser) (insertionMode, bool)

// useTheRulesFor runs the delegate insertionMode over p, returning the actual
// insertionMode unless the delegate caused a state transition.
// Section 11.2.3.1, "using the rules for".
func useTheRulesFor(p *parser, actual, delegate insertionMode) (insertionMode, bool) {
	im, consumed := delegate(p)
	if p.originalIM == delegate {
		p.originalIM = actual
	}
	if im != delegate {
		return im, consumed
	}
	return actual, consumed
}

// setOriginalIM sets the insertion mode to return to after completing a text or
// inTableText insertion mode.
// Section 11.2.3.1, "using the rules for".
func (p *parser) setOriginalIM(im insertionMode) {
	if p.originalIM != nil {
		panic("html: bad parser state: originalIM was set twice")
	}
	p.originalIM = im
}

// Section 11.2.3.1, "reset the insertion mode".
func (p *parser) resetInsertionMode() insertionMode {
	for i := len(p.oe) - 1; i >= 0; i-- {
		n := p.oe[i]
		if i == 0 {
			// TODO: set n to the context element, for HTML fragment parsing.
		}
		switch n.Data {
		case "select":
			return inSelectIM
		case "td", "th":
			return inCellIM
		case "tr":
			return inRowIM
		case "tbody", "thead", "tfoot":
			return inTableBodyIM
		case "caption":
			// TODO: return inCaptionIM
		case "colgroup":
			return inColumnGroupIM
		case "table":
			return inTableIM
		case "head":
			return inBodyIM
		case "body":
			return inBodyIM
		case "frameset":
			return inFramesetIM
		case "html":
			return beforeHeadIM
		}
	}
	return inBodyIM
}

// Section 11.2.5.4.1.
func initialIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case CommentToken:
		p.doc.Add(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return initialIM, true
	case DoctypeToken:
		p.doc.Add(&Node{
			Type: DoctypeNode,
			Data: p.tok.Data,
		})
		return beforeHTMLIM, true
	}
	// TODO: set "quirks mode"? It's defined in the DOM spec instead of HTML5 proper,
	// and so switching on "quirks mode" might belong in a different package.
	return beforeHTMLIM, false
}

// Section 11.2.5.4.2.
func beforeHTMLIM(p *parser) (insertionMode, bool) {
	var (
		add     bool
		attr    []Attribute
		implied bool
	)
	switch p.tok.Type {
	case ErrorToken:
		implied = true
	case TextToken:
		// TODO: distinguish whitespace text from others.
		implied = true
	case StartTagToken:
		if p.tok.Data == "html" {
			add = true
			attr = p.tok.Attr
		} else {
			implied = true
		}
	case EndTagToken:
		switch p.tok.Data {
		case "head", "body", "html", "br":
			implied = true
		default:
			// Ignore the token.
		}
	case CommentToken:
		p.doc.Add(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return beforeHTMLIM, true
	}
	if add || implied {
		p.addElement("html", attr)
	}
	return beforeHeadIM, !implied
}

// Section 11.2.5.4.3.
func beforeHeadIM(p *parser) (insertionMode, bool) {
	var (
		add     bool
		attr    []Attribute
		implied bool
	)
	switch p.tok.Type {
	case ErrorToken:
		implied = true
	case TextToken:
		// TODO: distinguish whitespace text from others.
		implied = true
	case StartTagToken:
		switch p.tok.Data {
		case "head":
			add = true
			attr = p.tok.Attr
		case "html":
			return useTheRulesFor(p, beforeHeadIM, inBodyIM)
		default:
			implied = true
		}
	case EndTagToken:
		switch p.tok.Data {
		case "head", "body", "html", "br":
			implied = true
		default:
			// Ignore the token.
		}
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return beforeHeadIM, true
	}
	if add || implied {
		p.addElement("head", attr)
		p.head = p.top()
	}
	return inHeadIM, !implied
}

const whitespace = " \t\r\n\f"

// Section 11.2.5.4.4.
func inHeadIM(p *parser) (insertionMode, bool) {
	var (
		pop     bool
		implied bool
	)
	switch p.tok.Type {
	case ErrorToken:
		implied = true
	case TextToken:
		s := strings.TrimLeft(p.tok.Data, whitespace)
		if len(s) < len(p.tok.Data) {
			// Add the initial whitespace to the current node.
			p.addText(p.tok.Data[:len(p.tok.Data)-len(s)])
			if s == "" {
				return inHeadIM, true
			}
			p.tok.Data = s
		}
		implied = true
	case StartTagToken:
		switch p.tok.Data {
		case "base", "basefont", "bgsound", "command", "link", "meta":
			p.addElement(p.tok.Data, p.tok.Attr)
			p.oe.pop()
			p.acknowledgeSelfClosingTag()
		case "script", "title", "noscript", "noframes", "style":
			p.addElement(p.tok.Data, p.tok.Attr)
			p.setOriginalIM(inHeadIM)
			return textIM, true
		default:
			implied = true
		}
	case EndTagToken:
		switch p.tok.Data {
		case "head":
			pop = true
		case "body", "html", "br":
			implied = true
		default:
			// Ignore the token.
			return inHeadIM, true
		}
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return inHeadIM, true
	}
	if pop || implied {
		n := p.oe.pop()
		if n.Data != "head" {
			panic("html: bad parser state: <head> element not found, in the in-head insertion mode")
		}
		return afterHeadIM, !implied
	}
	return inHeadIM, true
}

// Section 11.2.5.4.6.
func afterHeadIM(p *parser) (insertionMode, bool) {
	var (
		add        bool
		attr       []Attribute
		framesetOK bool
		implied    bool
	)
	switch p.tok.Type {
	case ErrorToken, TextToken:
		implied = true
		framesetOK = true
	case StartTagToken:
		switch p.tok.Data {
		case "html":
			// TODO.
		case "body":
			add = true
			attr = p.tok.Attr
			framesetOK = false
		case "frameset":
			p.addElement(p.tok.Data, p.tok.Attr)
			return inFramesetIM, true
		case "base", "basefont", "bgsound", "link", "meta", "noframes", "script", "style", "title":
			p.oe = append(p.oe, p.head)
			defer p.oe.pop()
			return useTheRulesFor(p, afterHeadIM, inHeadIM)
		case "head":
			// TODO.
		default:
			implied = true
			framesetOK = true
		}
	case EndTagToken:
		switch p.tok.Data {
		case "body", "html", "br":
			implied = true
			framesetOK = true
		default:
			// Ignore the token.
			return afterHeadIM, true
		}
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return afterHeadIM, true
	}
	if add || implied {
		p.addElement("body", attr)
		p.framesetOK = framesetOK
	}
	return inBodyIM, !implied
}

// copyAttributes copies attributes of src not found on dst to dst.
func copyAttributes(dst *Node, src Token) {
	if len(src.Attr) == 0 {
		return
	}
	attr := map[string]string{}
	for _, a := range dst.Attr {
		attr[a.Key] = a.Val
	}
	for _, a := range src.Attr {
		if _, ok := attr[a.Key]; !ok {
			dst.Attr = append(dst.Attr, a)
			attr[a.Key] = a.Val
		}
	}
}

// Section 11.2.5.4.7.
func inBodyIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case TextToken:
		p.reconstructActiveFormattingElements()
		p.addText(p.tok.Data)
		p.framesetOK = false
	case StartTagToken:
		switch p.tok.Data {
		case "address", "article", "aside", "blockquote", "center", "details", "dir", "div", "dl", "fieldset", "figcaption", "figure", "footer", "header", "hgroup", "menu", "nav", "ol", "p", "section", "summary", "ul":
			p.popUntil(buttonScopeStopTags, "p")
			p.addElement(p.tok.Data, p.tok.Attr)
		case "h1", "h2", "h3", "h4", "h5", "h6":
			p.popUntil(buttonScopeStopTags, "p")
			switch n := p.top(); n.Data {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				p.oe.pop()
			}
			p.addElement(p.tok.Data, p.tok.Attr)
		case "a":
			for i := len(p.afe) - 1; i >= 0 && p.afe[i].Type != scopeMarkerNode; i-- {
				if n := p.afe[i]; n.Type == ElementNode && n.Data == "a" {
					p.inBodyEndTagFormatting("a")
					p.oe.remove(n)
					p.afe.remove(n)
					break
				}
			}
			p.reconstructActiveFormattingElements()
			p.addFormattingElement(p.tok.Data, p.tok.Attr)
		case "b", "big", "code", "em", "font", "i", "s", "small", "strike", "strong", "tt", "u":
			p.reconstructActiveFormattingElements()
			p.addFormattingElement(p.tok.Data, p.tok.Attr)
		case "applet", "marquee", "object":
			p.reconstructActiveFormattingElements()
			p.addElement(p.tok.Data, p.tok.Attr)
			p.afe = append(p.afe, &scopeMarker)
			p.framesetOK = false
		case "area", "br", "embed", "img", "input", "keygen", "wbr":
			p.reconstructActiveFormattingElements()
			p.addElement(p.tok.Data, p.tok.Attr)
			p.oe.pop()
			p.acknowledgeSelfClosingTag()
			p.framesetOK = false
		case "table":
			p.popUntil(buttonScopeStopTags, "p") // TODO: skip this step in quirks mode.
			p.addElement(p.tok.Data, p.tok.Attr)
			p.framesetOK = false
			return inTableIM, true
		case "hr":
			p.popUntil(buttonScopeStopTags, "p")
			p.addElement(p.tok.Data, p.tok.Attr)
			p.oe.pop()
			p.acknowledgeSelfClosingTag()
			p.framesetOK = false
		case "select":
			p.reconstructActiveFormattingElements()
			p.addElement(p.tok.Data, p.tok.Attr)
			p.framesetOK = false
			// TODO: detect <select> inside a table.
			return inSelectIM, true
		case "li":
			p.framesetOK = false
			for i := len(p.oe) - 1; i >= 0; i-- {
				node := p.oe[i]
				switch node.Data {
				case "li":
					p.popUntil(listItemScopeStopTags, "li")
				case "address", "div", "p":
					continue
				default:
					if !isSpecialElement[node.Data] {
						continue
					}
				}
				break
			}
			p.popUntil(buttonScopeStopTags, "p")
			p.addElement(p.tok.Data, p.tok.Attr)
		case "optgroup", "option":
			if p.top().Data == "option" {
				p.oe.pop()
			}
			p.reconstructActiveFormattingElements()
			p.addElement(p.tok.Data, p.tok.Attr)
		case "body":
			if len(p.oe) >= 2 {
				body := p.oe[1]
				if body.Type == ElementNode && body.Data == "body" {
					p.framesetOK = false
					copyAttributes(body, p.tok)
				}
			}
		case "base", "basefont", "bgsound", "command", "link", "meta", "noframes", "script", "style", "title":
			return useTheRulesFor(p, inBodyIM, inHeadIM)
		case "image":
			p.tok.Data = "img"
			return inBodyIM, false
		case "caption", "col", "colgroup", "frame", "head", "tbody", "td", "tfoot", "th", "thead", "tr":
			// Ignore the token.
		default:
			// TODO.
			p.addElement(p.tok.Data, p.tok.Attr)
		}
	case EndTagToken:
		switch p.tok.Data {
		case "body":
			// TODO: autoclose the stack of open elements.
			return afterBodyIM, true
		case "p":
			if !p.elementInScope(buttonScopeStopTags, "p") {
				p.addElement("p", nil)
			}
			p.popUntil(buttonScopeStopTags, "p")
		case "a", "b", "big", "code", "em", "font", "i", "nobr", "s", "small", "strike", "strong", "tt", "u":
			p.inBodyEndTagFormatting(p.tok.Data)
		case "address", "article", "aside", "blockquote", "button", "center", "details", "dir", "div", "dl", "fieldset", "figcaption", "figure", "footer", "header", "hgroup", "listing", "menu", "nav", "ol", "pre", "section", "summary", "ul":
			p.popUntil(defaultScopeStopTags, p.tok.Data)
		case "applet", "marquee", "object":
			if p.popUntil(defaultScopeStopTags, p.tok.Data) {
				p.clearActiveFormattingElements()
			}
		default:
			p.inBodyEndTagOther(p.tok.Data)
		}
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
	}

	return inBodyIM, true
}

func (p *parser) inBodyEndTagFormatting(tag string) {
	// This is the "adoption agency" algorithm, described at
	// http://www.whatwg.org/specs/web-apps/current-work/multipage/tokenization.html#adoptionAgency

	// TODO: this is a fairly literal line-by-line translation of that algorithm.
	// Once the code successfully parses the comprehensive test suite, we should
	// refactor this code to be more idiomatic.

	// Steps 1-3. The outer loop.
	for i := 0; i < 8; i++ {
		// Step 4. Find the formatting element.
		var formattingElement *Node
		for j := len(p.afe) - 1; j >= 0; j-- {
			if p.afe[j].Type == scopeMarkerNode {
				break
			}
			if p.afe[j].Data == tag {
				formattingElement = p.afe[j]
				break
			}
		}
		if formattingElement == nil {
			p.inBodyEndTagOther(tag)
			return
		}
		feIndex := p.oe.index(formattingElement)
		if feIndex == -1 {
			p.afe.remove(formattingElement)
			return
		}
		if !p.elementInScope(defaultScopeStopTags, tag) {
			// Ignore the tag.
			return
		}

		// Steps 5-6. Find the furthest block.
		var furthestBlock *Node
		for _, e := range p.oe[feIndex:] {
			if isSpecialElement[e.Data] {
				furthestBlock = e
				break
			}
		}
		if furthestBlock == nil {
			e := p.oe.pop()
			for e != formattingElement {
				e = p.oe.pop()
			}
			p.afe.remove(e)
			return
		}

		// Steps 7-8. Find the common ancestor and bookmark node.
		commonAncestor := p.oe[feIndex-1]
		bookmark := p.afe.index(formattingElement)

		// Step 9. The inner loop. Find the lastNode to reparent.
		lastNode := furthestBlock
		node := furthestBlock
		x := p.oe.index(node)
		// Steps 9.1-9.3.
		for j := 0; j < 3; j++ {
			// Step 9.4.
			x--
			node = p.oe[x]
			// Step 9.5.
			if p.afe.index(node) == -1 {
				p.oe.remove(node)
				continue
			}
			// Step 9.6.
			if node == formattingElement {
				break
			}
			// Step 9.7.
			clone := node.clone()
			p.afe[p.afe.index(node)] = clone
			p.oe[p.oe.index(node)] = clone
			node = clone
			// Step 9.8.
			if lastNode == furthestBlock {
				bookmark = p.afe.index(node) + 1
			}
			// Step 9.9.
			if lastNode.Parent != nil {
				lastNode.Parent.Remove(lastNode)
			}
			node.Add(lastNode)
			// Step 9.10.
			lastNode = node
		}

		// Step 10. Reparent lastNode to the common ancestor,
		// or for misnested table nodes, to the foster parent.
		if lastNode.Parent != nil {
			lastNode.Parent.Remove(lastNode)
		}
		switch commonAncestor.Data {
		case "table", "tbody", "tfoot", "thead", "tr":
			p.fosterParent(lastNode)
		default:
			commonAncestor.Add(lastNode)
		}

		// Steps 11-13. Reparent nodes from the furthest block's children
		// to a clone of the formatting element.
		clone := formattingElement.clone()
		reparentChildren(clone, furthestBlock)
		furthestBlock.Add(clone)

		// Step 14. Fix up the list of active formatting elements.
		if oldLoc := p.afe.index(formattingElement); oldLoc != -1 && oldLoc < bookmark {
			// Move the bookmark with the rest of the list.
			bookmark--
		}
		p.afe.remove(formattingElement)
		p.afe.insert(bookmark, clone)

		// Step 15. Fix up the stack of open elements.
		p.oe.remove(formattingElement)
		p.oe.insert(p.oe.index(furthestBlock)+1, clone)
	}
}

// inBodyEndTagOther performs the "any other end tag" algorithm for inBodyIM.
func (p *parser) inBodyEndTagOther(tag string) {
	for i := len(p.oe) - 1; i >= 0; i-- {
		if p.oe[i].Data == tag {
			p.oe = p.oe[:i]
			break
		}
		if isSpecialElement[p.oe[i].Data] {
			break
		}
	}
}

// Section 11.2.5.4.8.
func textIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case ErrorToken:
		p.oe.pop()
	case TextToken:
		p.addText(p.tok.Data)
		return textIM, true
	case EndTagToken:
		p.oe.pop()
	}
	o := p.originalIM
	p.originalIM = nil
	return o, p.tok.Type == EndTagToken
}

// Section 11.2.5.4.9.
func inTableIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case ErrorToken:
		// Stop parsing.
		return nil, true
	case TextToken:
		// TODO.
	case StartTagToken:
		switch p.tok.Data {
		case "tbody", "tfoot", "thead":
			p.clearStackToContext(tableScopeStopTags)
			p.addElement(p.tok.Data, p.tok.Attr)
			return inTableBodyIM, true
		case "td", "th", "tr":
			p.clearStackToContext(tableScopeStopTags)
			p.addElement("tbody", nil)
			return inTableBodyIM, false
		case "table":
			if p.popUntil(tableScopeStopTags, "table") {
				return p.resetInsertionMode(), false
			}
			// Ignore the token.
			return inTableIM, true
		case "colgroup":
			p.clearStackToContext(tableScopeStopTags)
			p.addElement(p.tok.Data, p.tok.Attr)
			return inColumnGroupIM, true
		case "col":
			p.clearStackToContext(tableScopeStopTags)
			p.addElement("colgroup", p.tok.Attr)
			return inColumnGroupIM, false
		default:
			// TODO.
		}
	case EndTagToken:
		switch p.tok.Data {
		case "table":
			if p.popUntil(tableScopeStopTags, "table") {
				return p.resetInsertionMode(), true
			}
			// Ignore the token.
			return inTableIM, true
		case "body", "caption", "col", "colgroup", "html", "tbody", "td", "tfoot", "th", "thead", "tr":
			// Ignore the token.
			return inTableIM, true
		}
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return inTableIM, true
	}

	switch p.top().Data {
	case "table", "tbody", "tfoot", "thead", "tr":
		p.fosterParenting = true
		defer func() { p.fosterParenting = false }()
	}

	return useTheRulesFor(p, inTableIM, inBodyIM)
}

// clearStackToContext pops elements off the stack of open elements
// until an element listed in stopTags is found.
func (p *parser) clearStackToContext(stopTags []string) {
	for i := len(p.oe) - 1; i >= 0; i-- {
		for _, tag := range stopTags {
			if p.oe[i].Data == tag {
				p.oe = p.oe[:i+1]
				return
			}
		}
	}
}

// Section 11.2.5.4.12.
func inColumnGroupIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return inColumnGroupIM, true
	case DoctypeToken:
		// Ignore the token.
		return inColumnGroupIM, true
	case StartTagToken:
		switch p.tok.Data {
		case "html":
			return useTheRulesFor(p, inColumnGroupIM, inBodyIM)
		case "col":
			p.addElement(p.tok.Data, p.tok.Attr)
			p.oe.pop()
			p.acknowledgeSelfClosingTag()
			return inColumnGroupIM, true
		}
	case EndTagToken:
		switch p.tok.Data {
		case "colgroup":
			if p.oe.top().Data != "html" {
				p.oe.pop()
			}
			return inTableIM, true
		case "col":
			// Ignore the token.
			return inColumnGroupIM, true
		}
	}
	if p.oe.top().Data != "html" {
		p.oe.pop()
	}
	return inTableIM, false
}

// Section 11.2.5.4.13.
func inTableBodyIM(p *parser) (insertionMode, bool) {
	var (
		add      bool
		data     string
		attr     []Attribute
		consumed bool
	)
	switch p.tok.Type {
	case ErrorToken:
		// TODO.
	case TextToken:
		// TODO.
	case StartTagToken:
		switch p.tok.Data {
		case "tr":
			add = true
			data = p.tok.Data
			attr = p.tok.Attr
			consumed = true
		case "td", "th":
			add = true
			data = "tr"
			consumed = false
		default:
			// TODO.
		}
	case EndTagToken:
		switch p.tok.Data {
		case "table":
			if p.popUntil(tableScopeStopTags, "tbody", "thead", "tfoot") {
				return inTableIM, false
			}
			// Ignore the token.
			return inTableBodyIM, true
		case "body", "caption", "col", "colgroup", "html", "td", "th", "tr":
			// Ignore the token.
			return inTableBodyIM, true
		}
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return inTableBodyIM, true
	}
	if add {
		// TODO: clear the stack back to a table body context.
		p.addElement(data, attr)
		return inRowIM, consumed
	}
	return useTheRulesFor(p, inTableBodyIM, inTableIM)
}

// Section 11.2.5.4.14.
func inRowIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case ErrorToken:
		// TODO.
	case TextToken:
		// TODO.
	case StartTagToken:
		switch p.tok.Data {
		case "td", "th":
			p.clearStackToContext(tableRowContextStopTags)
			p.addElement(p.tok.Data, p.tok.Attr)
			p.afe = append(p.afe, &scopeMarker)
			return inCellIM, true
		case "caption", "col", "colgroup", "tbody", "tfoot", "thead", "tr":
			if p.popUntil(tableScopeStopTags, "tr") {
				return inTableBodyIM, false
			}
			// Ignore the token.
			return inRowIM, true
		default:
			// TODO.
		}
	case EndTagToken:
		switch p.tok.Data {
		case "tr":
			if p.popUntil(tableScopeStopTags, "tr") {
				return inTableBodyIM, true
			}
			// Ignore the token.
			return inRowIM, true
		case "table":
			if p.popUntil(tableScopeStopTags, "tr") {
				return inTableBodyIM, false
			}
			// Ignore the token.
			return inRowIM, true
		case "tbody", "tfoot", "thead":
			// TODO.
		case "body", "caption", "col", "colgroup", "html", "td", "th":
			// Ignore the token.
			return inRowIM, true
		default:
			// TODO.
		}
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return inRowIM, true
	}
	return useTheRulesFor(p, inRowIM, inTableIM)
}

// Section 11.2.5.4.15.
func inCellIM(p *parser) (insertionMode, bool) {
	var (
		closeTheCellAndReprocess bool
	)
	switch p.tok.Type {
	case StartTagToken:
		switch p.tok.Data {
		case "caption", "col", "colgroup", "tbody", "td", "tfoot", "th", "thead", "tr":
			// TODO: check for "td" or "th" in table scope.
			closeTheCellAndReprocess = true
		}
	case EndTagToken:
		switch p.tok.Data {
		case "td", "th":
			if !p.popUntil(tableScopeStopTags, p.tok.Data) {
				// Ignore the token.
				return inCellIM, true
			}
			p.clearActiveFormattingElements()
			return inRowIM, true
		case "body", "caption", "col", "colgroup", "html":
			// TODO.
		case "table", "tbody", "tfoot", "thead", "tr":
			// TODO: check for matching element in table scope.
			closeTheCellAndReprocess = true
		}
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return inCellIM, true
	}
	if closeTheCellAndReprocess {
		if p.popUntil(tableScopeStopTags, "td") || p.popUntil(tableScopeStopTags, "th") {
			p.clearActiveFormattingElements()
			return inRowIM, false
		}
	}
	return useTheRulesFor(p, inCellIM, inBodyIM)
}

// Section 11.2.5.4.16.
func inSelectIM(p *parser) (insertionMode, bool) {
	endSelect := false
	switch p.tok.Type {
	case ErrorToken:
		// TODO.
	case TextToken:
		p.addText(p.tok.Data)
	case StartTagToken:
		switch p.tok.Data {
		case "html":
			// TODO.
		case "option":
			if p.top().Data == "option" {
				p.oe.pop()
			}
			p.addElement(p.tok.Data, p.tok.Attr)
		case "optgroup":
			// TODO.
		case "select":
			endSelect = true
		case "input", "keygen", "textarea":
			// TODO.
		case "script":
			// TODO.
		default:
			// Ignore the token.
		}
	case EndTagToken:
		switch p.tok.Data {
		case "option":
			// TODO.
		case "optgroup":
			// TODO.
		case "select":
			endSelect = true
		default:
			// Ignore the token.
		}
	case CommentToken:
		p.doc.Add(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
	}
	if endSelect {
		for i := len(p.oe) - 1; i >= 0; i-- {
			switch p.oe[i].Data {
			case "select":
				p.oe = p.oe[:i]
				return p.resetInsertionMode(), true
			case "option", "optgroup":
				continue
			default:
				// Ignore the token.
				return inSelectIM, true
			}
		}
	}
	return inSelectIM, true
}

// Section 11.2.5.4.18.
func afterBodyIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case ErrorToken:
		// TODO.
	case TextToken:
		// TODO.
	case StartTagToken:
		// TODO.
	case EndTagToken:
		switch p.tok.Data {
		case "html":
			// TODO: autoclose the stack of open elements.
			return afterAfterBodyIM, true
		default:
			// TODO.
		}
	case CommentToken:
		// The comment is attached to the <html> element.
		if len(p.oe) < 1 || p.oe[0].Data != "html" {
			panic("html: bad parser state: <html> element not found, in the after-body insertion mode")
		}
		p.oe[0].Add(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return afterBodyIM, true
	}
	// TODO: should this be "return inBodyIM, true"?
	return afterBodyIM, true
}

// Section 11.2.5.4.19.
func inFramesetIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
	case StartTagToken:
		switch p.tok.Data {
		case "html":
			return useTheRulesFor(p, inFramesetIM, inBodyIM)
		case "frameset":
			p.addElement(p.tok.Data, p.tok.Attr)
		case "frame":
			p.addElement(p.tok.Data, p.tok.Attr)
			p.oe.pop()
			p.acknowledgeSelfClosingTag()
		case "noframes":
			return useTheRulesFor(p, inFramesetIM, inHeadIM)
		}
	case EndTagToken:
		switch p.tok.Data {
		case "frameset":
			if p.oe.top().Data != "html" {
				p.oe.pop()
				if p.oe.top().Data != "frameset" {
					return afterFramesetIM, true
				}
			}
		}
	default:
		// Ignore the token.
	}
	return inFramesetIM, true
}

// Section 11.2.5.4.20.
func afterFramesetIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
	case StartTagToken:
		switch p.tok.Data {
		case "html":
			return useTheRulesFor(p, inFramesetIM, inBodyIM)
		case "noframes":
			return useTheRulesFor(p, inFramesetIM, inHeadIM)
		}
	case EndTagToken:
		switch p.tok.Data {
		case "html":
			return afterAfterFramesetIM, true
		}
	default:
		// Ignore the token.
	}
	return afterFramesetIM, true
}

// Section 11.2.5.4.21.
func afterAfterBodyIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case ErrorToken:
		// Stop parsing.
		return nil, true
	case TextToken:
		// TODO.
	case StartTagToken:
		if p.tok.Data == "html" {
			return useTheRulesFor(p, afterAfterBodyIM, inBodyIM)
		}
	case CommentToken:
		p.doc.Add(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
		return afterAfterBodyIM, true
	}
	return inBodyIM, false
}

// Section 11.2.5.4.22.
func afterAfterFramesetIM(p *parser) (insertionMode, bool) {
	switch p.tok.Type {
	case CommentToken:
		p.addChild(&Node{
			Type: CommentNode,
			Data: p.tok.Data,
		})
	case StartTagToken:
		switch p.tok.Data {
		case "html":
			return useTheRulesFor(p, afterAfterFramesetIM, inBodyIM)
		case "noframes":
			return useTheRulesFor(p, afterAfterFramesetIM, inHeadIM)
		}
	default:
		// Ignore the token.
	}
	return afterAfterFramesetIM, true
}

// Parse returns the parse tree for the HTML from the given Reader.
// The input is assumed to be UTF-8 encoded.
func Parse(r io.Reader) (*Node, error) {
	p := &parser{
		tokenizer: NewTokenizer(r),
		doc: &Node{
			Type: DocumentNode,
		},
		scripting:  true,
		framesetOK: true,
	}
	// Iterate until EOF. Any other error will cause an early return.
	im, consumed := initialIM, true
	for {
		if consumed {
			if err := p.read(); err != nil {
				if err == io.EOF {
					break
				}
				return nil, err
			}
		}
		im, consumed = im(p)
	}
	// Loop until the final token (the ErrorToken signifying EOF) is consumed.
	for {
		if im, consumed = im(p); consumed {
			break
		}
	}
	return p.doc, nil
}
