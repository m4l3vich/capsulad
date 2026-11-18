// Line-oriented Markdown for the application guide pane.
//
// The renderer returns a block model, never an HTML string. The Vue template
// interpolates every text field, so package-authored content cannot become
// markup, a script, a link target or a network request. Only the structures a
// guide needs are recognized: headings, paragraphs, lists, fenced code and
// pipe tables. Everything else stays visible text.

const HEADING = /^(#{1,6})\s+(.*)$/
const BULLET = /^\s*[-*+]\s+(.*)$/
const ORDERED = /^\s*\d+[.)]\s+(.*)$/
const FENCE = /^\s*```/
const RULE = /^\s*(?:-{3,}|\*{3,}|_{3,})\s*$/
const TABLE_SEPARATOR = /^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$/

function inlineText(text) {
  return String(text)
    .replace(/`([^`]+)`/g, '$1')
    .replace(/\*\*([^*]+)\*\*/g, '$1')
    .replace(/__([^_]+)__/g, '$1')
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]+)\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g, '$1 ($2)')
    .trim()
}

function tableCells(line) {
  let row = line.trim()
  if (row.startsWith('|')) row = row.slice(1)
  if (row.endsWith('|')) row = row.slice(0, -1)
  return row.split('|').map((cell) => inlineText(cell))
}

function padRow(row, width) {
  while (row.length < width) row.push('')
  return row.slice(0, width)
}

export function guideBlocks(markdown) {
  const source = String(markdown ?? '').replace(/^\ufeff/, '').replace(/\r\n?/g, '\n')
  const lines = source.split('\n')
  const blocks = []
  let paragraph = []
  let list = null
  let code = null

  const flushParagraph = () => {
    if (!paragraph.length) return
    const text = inlineText(paragraph.join(' '))
    if (text) blocks.push({ type: 'paragraph', text })
    paragraph = []
  }
  const flushList = () => {
    if (list) blocks.push(list)
    list = null
  }
  const flushCode = () => {
    if (!code) return
    blocks.push({ type: 'code', text: code.lines.join('\n') })
    code = null
  }

  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i]

    if (code) {
      if (FENCE.test(line)) flushCode()
      else code.lines.push(line)
      continue
    }

    if (FENCE.test(line)) {
      flushParagraph()
      flushList()
      code = { lines: [] }
      continue
    }

    if (!line.trim()) {
      flushParagraph()
      flushList()
      continue
    }

    if (RULE.test(line)) {
      flushParagraph()
      flushList()
      continue
    }

    const heading = HEADING.exec(line)
    if (heading) {
      flushParagraph()
      flushList()
      const text = inlineText(heading[2])
      if (text) blocks.push({ type: 'heading', level: heading[1].length, text })
      continue
    }

    if (line.includes('|') && i + 1 < lines.length && TABLE_SEPARATOR.test(lines[i + 1])) {
      flushParagraph()
      flushList()
      const headers = tableCells(line)
      const rows = []
      i += 2
      while (i < lines.length && lines[i].trim() && lines[i].includes('|')) {
        rows.push(padRow(tableCells(lines[i]), headers.length))
        i += 1
      }
      i -= 1
      blocks.push({ type: 'table', headers, rows })
      continue
    }

    const bullet = BULLET.exec(line)
    const ordered = bullet ? null : ORDERED.exec(line)
    if (bullet || ordered) {
      flushParagraph()
      const kind = bullet ? 'ul' : 'ol'
      if (!list || list.kind !== kind) {
        flushList()
        list = { type: 'list', kind, items: [] }
      }
      const item = inlineText((bullet || ordered)[1])
      if (item) list.items.push(item)
      continue
    }

    // An indented continuation belongs to the last list item; a new
    // unindented line ends the list and starts a paragraph.
    if (list && /^\s{2,}\S/.test(line)) {
      list.items[list.items.length - 1] += ' ' + inlineText(line)
      continue
    }
    if (list) flushList()
    paragraph.push(line.trim())
  }

  flushParagraph()
  flushList()
  flushCode()
  return blocks
}