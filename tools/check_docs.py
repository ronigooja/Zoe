#!/usr/bin/env python3
"""Check documentation structure, English prose, and local navigation.

These checks establish structural consistency, not semantic correctness.
Only the Python standard library is required; no Git history is needed.
"""
from __future__ import annotations

import argparse
import re
import sys
import unicodedata
from collections import Counter
from pathlib import Path
from urllib.parse import unquote, urlsplit

NUMBERED_HEADING_RE = re.compile(r"^#{1,6}\s+(.*)$")
MARKDOWN_LINK_RE = re.compile(r"!?\[[^\]]*\]\((<[^>]*>|[^)]*)\)")
REFERENCE_LINK_RE = re.compile(r"!?\[([^\]]+)\](?:\[([^\]]*)\])")
REFERENCE_DEFINITION_RE = re.compile(r"^\s{0,3}\[([^\]]+)\]:\s*(<[^>]+>|\S+)", re.M)
HTML_ID_RE = re.compile(r"\bid\s*=\s*[\"']([^\"']+)[\"']", re.I)
FENCE_RE = re.compile(r"^\s{0,3}(```+|~~~+)")

PARAGRAPH_LINE_LIMIT = 1200
TABLE_CELL_LIMIT = 600
REQUIRED_DOCUMENTS = (
    "AGENTS.md", "README.md", "docs/design/data-flows.md",
    "docs/development.md", "docs/operations.md",
)
REQUIRED_COMPONENTS = (
    "register", "steward", "execution", "results", "screen", "memory", "media", "configuration",
)
CONTRACT_HEADINGS = (
    "Purpose and boundary", "Inputs and outputs", "Required behavior",
    "Persistence and idempotency", "Failure and recovery",
    "Implementation locations", "Verification obligations",
)
RETIRED_DOCUMENTS = {
    "docs/product.md", "docs/architecture.md", "docs/master-plan.md",
    "docs/design/screen-document.md", "docs/design/target-architecture.md",
    "docs/design/recovery.md", "docs/development/verification.md",
    "docs/operations/ao-host-removal.md", "docs/operations/model-menu.md",
}
RETIRED_PREFIXES = ("docs/architecture/decisions/", "docs/acceptance/")
HISTORY_PARTS = {"archive", "archives", "archived", "history"}


def unfenced_lines(text: str):
    """Yield numbered lines outside fences using the same rules for every scan."""
    state: tuple[str, int] | None = None
    for number, line in enumerate(text.splitlines(), 1):
        fence = FENCE_RE.match(line)
        if state is not None:
            if (fence and fence.group(1)[0] == state[0]
                    and len(fence.group(1)) >= state[1]
                    and not line[fence.end():].strip()):
                state = None
            continue
        if fence:
            marker = fence.group(1)
            state = (marker[0], len(marker))
            continue
        yield number, line


def strip_markdown_for_scan(text: str, *, quoted: bool = False) -> str:
    output: list[str] = []
    for _, line in unfenced_lines(text):
        if quoted and re.match(r"^\s*>\s?", line):
            continue
        # Inline code and HTML comments commonly hold source literals or paths.
        line = re.sub(r"`+[^`]*`+", "", line)
        line = re.sub(r"\]\([^)]*\)", ")", line)
        line = re.sub(r"[\w./-]*[\u3400-\u9fff][\w./-]*\.md\b", "", line)
        line = re.sub(r"<!--.*?-->", "", line)
        output.append(line)
    return "\n".join(output)


def has_cjk(text: str) -> bool:
    for char in text:
        code = ord(char)
        if (
            0x3400 <= code <= 0x4DBF
            or 0x4E00 <= code <= 0x9FFF
            or 0xF900 <= code <= 0xFAFF
            or 0x3040 <= code <= 0x30FF
            or 0xAC00 <= code <= 0xD7AF
        ):
            return True
    return False


def markdown_files(root: Path) -> list[Path]:
    return sorted(
        path for path in root.rglob("*.md")
        if not any(part in {".git", ".venv", "node_modules", ".build", "__pycache__"} for part in path.parts)
    )


def cjk_check(root: Path) -> list[str]:
    errors = []
    for path in markdown_files(root):
        cleaned = strip_markdown_for_scan(path.read_text(encoding="utf-8"), quoted=True)
        if has_cjk(cleaned):
            errors.append(f"remaining CJK narrative text: {path.relative_to(root)}")
    return errors


def slugify(heading: str) -> str:
    # GitHub heading anchors remove markup and punctuation, preserve underscores
    # and hyphens, lowercase, then replace whitespace with hyphens.
    value = re.sub(r"!?\[([^\]]+)\]\([^)]*\)", r"\1", heading)
    value = re.sub(r"<[^>]+>", "", value)
    value = re.sub(r"`+([^`]+)`+", r"\1", value)
    # GitHub's slugger removes the em dash but retains each adjacent
    # whitespace boundary. Keep a sentinel while collapsing other markup so
    # "repair — 2026" becomes "repair--2026" rather than one hyphen.
    value = value.replace("—", "\x00").lower().strip()
    value = "".join(
        char for char in value
        if char in "-_ \x00" or char.isalnum() or unicodedata.category(char)[0] in "LM"
    )
    return re.sub(r"\s+", "-", value).replace("\x00", "")


def heading_anchors(text: str) -> set[str]:
    anchors: set[str] = set()
    counts: Counter[str] = Counter()
    for _, line in unfenced_lines(text):
        heading = NUMBERED_HEADING_RE.match(line)
        if heading:
            title = re.sub(r"\s+#+\s*$", "", heading.group(1)).strip()
            base = slugify(title)
            suffix = counts[base]
            counts[base] += 1
            anchors.add(base if suffix == 0 else f"{base}-{suffix}")
        for html_id in HTML_ID_RE.findall(line):
            anchors.add(html_id)
    return anchors


def split_link_destination(value: str) -> str:
    value = value.strip()
    if value.startswith("<") and value.endswith(">"):
        value = value[1:-1]
    # A title may follow a destination separated by whitespace.
    if value.startswith("<"):
        end = value.find(">")
        if end >= 0:
            return value[1:end]
    return value.split(None, 1)[0] if value else ""


def normalize_reference_id(value: str) -> str:
    return " ".join(value.split()).casefold()


def check_destination(
    root: Path,
    source: Path,
    destination: str,
    anchors_by_path: dict[Path, set[str]],
    errors: list[str],
) -> None:
    destination = split_link_destination(destination)
    if not destination:
        return
    decoded = unquote(destination)
    parsed = urlsplit(decoded)
    if parsed.scheme or parsed.netloc:
        return
    target_text = parsed.path
    fragment = parsed.fragment
    if target_text:
        target = (source.parent / target_text).resolve()
        if not target.exists():
            errors.append(
                f"broken local link in {source.relative_to(root)}: {destination}"
            )
            return
    else:
        target = source.resolve()
    if fragment and not target.is_dir() and target.suffix.lower() == ".md":
        anchors = anchors_by_path.get(target)
        if anchors is None:
            try:
                anchors = heading_anchors(target.read_text(encoding="utf-8"))
            except (OSError, UnicodeError):
                anchors = set()
        if fragment not in anchors:
            errors.append(
                f"broken Markdown heading link in {source.relative_to(root)}: {destination}"
            )


def link_check(root: Path) -> list[str]:
    errors: list[str] = []
    files = markdown_files(root)
    anchors_by_path = {path.resolve(): heading_anchors(path.read_text(encoding="utf-8")) for path in files}
    for source in files:
        text = navigation_text(source.read_text(encoding="utf-8"))
        definitions = {
            normalize_reference_id(identifier): destination
            for identifier, destination in REFERENCE_DEFINITION_RE.findall(text)
        }
        for destination in definitions.values():
            check_destination(root, source, destination, anchors_by_path, errors)
        for match in MARKDOWN_LINK_RE.finditer(text):
            check_destination(root, source, match.group(1), anchors_by_path, errors)
        for match in REFERENCE_LINK_RE.finditer(text):
            identifier = match.group(2) or match.group(1)
            destination = definitions.get(normalize_reference_id(identifier))
            if destination is None:
                errors.append(
                    f"unresolved Markdown link reference in {source.relative_to(root)}: [{identifier}]"
                )
            else:
                check_destination(root, source, destination, anchors_by_path, errors)
    return errors


def document_scope(root: Path) -> list[Path]:
    """Return root-level Markdown and Markdown below docs/."""
    return sorted(
        path for path in markdown_files(root)
        if path.parent == root or root / "docs" in path.parents
    )


def length_warnings(root: Path) -> list[str]:
    warnings: list[str] = []
    for path in document_scope(root):
        for number, line in unfenced_lines(path.read_text(encoding="utf-8")):
            stripped = line.strip()
            if (
                len(line) > PARAGRAPH_LINE_LIMIT
                and stripped
                and not stripped.startswith(("#", ">", "- ", "* ", "+ ", "|"))
                and not re.match(r"^\d+[.)]\s", stripped)
            ):
                warnings.append(
                    f"long paragraph line in {path.relative_to(root)}:{number} "
                    f"({len(line)} characters)"
                )
            if "|" in line and len(line) > TABLE_CELL_LIMIT:
                cells = line.split("|")[1:-1]
                for index, cell in enumerate(cells, 1):
                    if len(cell.strip()) > TABLE_CELL_LIMIT:
                        warnings.append(
                            f"long Markdown table cell in {path.relative_to(root)}:{number} "
                            f"cell {index} ({len(cell.strip())} characters)"
                        )
    return warnings


def inline_navigation(line: str) -> str:
    """Skip code spans without treating destination/title backticks as code."""
    if REFERENCE_DEFINITION_RE.match(line):
        return line
    output: list[str] = []
    index = 0
    while index < len(line):
        link = MARKDOWN_LINK_RE.match(line, index) or REFERENCE_LINK_RE.match(line, index)
        if link:
            output.append(link.group())
            index = link.end()
        elif line[index] == "\\" and index + 1 < len(line):
            output.append(line[index:index + 2])
            index += 2
        elif line[index] == "`":
            marker = re.match(r"`+", line[index:]).group()
            closing = re.search(r"(?<!`)" + marker + r"(?!`)", line[index + len(marker):])
            if closing:
                index += len(marker) + closing.end()
                output.append(" ")
            else:
                output.append(marker)
                index += len(marker)
        else:
            output.append(line[index])
            index += 1
    return "".join(output)


def navigation_text(text: str, *, keep_inline_code: bool = False) -> str:
    """Ignore example links in code and comments, but retain actual destinations."""
    text = re.sub(r"<!--.*?-->", "", text, flags=re.S)
    return "\n".join(line if keep_inline_code else inline_navigation(line)
                     for _, line in unfenced_lines(text))


def link_destinations(text: str, *, rendered_only: bool = False) -> list[str]:
    """Resolve used references; optionally include unused definitions for validation."""
    text = navigation_text(text)
    definitions = {normalize_reference_id(identifier): destination
                   for identifier, destination in REFERENCE_DEFINITION_RE.findall(text)}
    body = REFERENCE_DEFINITION_RE.sub("", text)
    destinations = [match.group(1) for match in MARKDOWN_LINK_RE.finditer(body)]
    body = MARKDOWN_LINK_RE.sub("", body)
    for match in REFERENCE_LINK_RE.finditer(body):
        destination = definitions.get(normalize_reference_id(match.group(2) or match.group(1)))
        if destination is not None:
            destinations.append(destination)
    if not rendered_only:
        destinations.extend(definitions.values())
    return destinations


def local_target(source: Path, destination: str) -> Path | None:
    parsed = urlsplit(unquote(split_link_destination(destination)))
    if parsed.scheme or parsed.netloc:
        return None
    return (source.parent / parsed.path).resolve() if parsed.path else source.resolve()


def structure_check(root: Path) -> list[str]:
    errors: list[str] = []
    required = list(REQUIRED_DOCUMENTS) + [
        f"docs/design/flows/{name}.md" for name in REQUIRED_COMPONENTS
    ]
    for relative in required:
        if not (root / relative).is_file():
            errors.append(f"missing required document: {relative}")

    flows = sorted((root / "docs/design/flows").rglob("*.md"))
    overview = root / "docs/design/data-flows.md"
    navigation = {
        root / "AGENTS.md": [overview, root / "docs/development.md", root / "docs/operations.md"],
        root / "README.md": [overview, root / "docs/development.md", root / "docs/operations.md"],
        overview: flows + [root / "docs/development.md", root / "docs/operations.md"],
    }
    for source, expected in navigation.items():
        if not source.is_file():
            continue
        targets = {local_target(source, value) for value in link_destinations(
            source.read_text(encoding="utf-8"), rendered_only=True)}
        for target in expected:
            if target.resolve() not in targets:
                errors.append(f"missing navigation in {source.relative_to(root)}: {target.relative_to(root)}")

    for path in flows:
        text = navigation_text(path.read_text(encoding="utf-8"), keep_inline_code=True)
        sections = list(re.finditer(r"^## (.+?)\s*#*\s*$", text, re.M))
        for title in CONTRACT_HEADINGS:
            matches = [(index, section) for index, section in enumerate(sections) if section.group(1) == title]
            if len(matches) != 1:
                errors.append(f"contract section {title!r} occurs {len(matches)} times in {path.relative_to(root)}; expected once")
                continue
            index, section = matches[0]
            end = sections[index + 1].start() if index + 1 < len(sections) else len(text)
            body = text[section.end():end]
            # Subheadings or Markdown punctuation alone do not fill a section.
            body = re.sub(r"^#{1,6}\s+.*$", "", body, flags=re.M)
            if not re.search(r"[A-Za-z0-9]", body):
                errors.append(f"empty contract section {title!r} in {path.relative_to(root)}")
    return errors


def authority_check(root: Path) -> list[str]:
    """Current documents cannot delegate normative navigation to retired/history files."""
    root = root.resolve()
    errors: list[str] = []
    for source in document_scope(root):
        if HISTORY_PARTS.intersection(source.relative_to(root).parts):
            continue
        text = source.read_text(encoding="utf-8")
        for destination in link_destinations(text):
            target = local_target(source, destination)
            if target is None or not target.is_relative_to(root):
                continue
            relative = target.relative_to(root)
            value = relative.as_posix()
            if value in RETIRED_DOCUMENTS or value.startswith(RETIRED_PREFIXES):
                errors.append(f"retired documentation link in {source.relative_to(root)}: {destination}")
            elif HISTORY_PARTS.intersection(relative.parts):
                errors.append(f"archive/history navigation in current document {source.relative_to(root)}: {destination}; use an immutable external Git link for historical context")
        # Bare repository paths (often inline code) must not preserve retired
        # instructions. Remove external URLs first: historical Git links are
        # context, not a dependency on a local normative document.
        literals = re.sub(r"(?:https?://|//)[^\s<>`]+", "", text)
        for value in sorted(set(re.findall(r"(?<![\w/])docs/[\w./-]+\.md\b", literals))):
            if value in RETIRED_DOCUMENTS or value.startswith(RETIRED_PREFIXES):
                errors.append(f"retired documentation reference in {source.relative_to(root)}: {value}")
    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parent.parent)
    parser.add_argument(
        "--strict-length", "--strict", dest="strict", action="store_true",
        help="treat documentation length warnings as errors",
    )
    args = parser.parse_args()
    root = args.root.resolve()
    failures = structure_check(root) + authority_check(root) + link_check(root) + cjk_check(root)
    warnings = length_warnings(root)
    for failure in failures:
        print(f"FAIL: {failure}")
    for warning in warnings:
        print(f"WARN: {warning}")
    if args.strict:
        failures.extend(warnings)
    if failures:
        print(f"documentation checks failed: {len(failures)} issue(s)")
        return 1
    print(
        "documentation checks passed (structure, navigation, English, local links; not semantic correctness)"
        f"; warnings: {len(warnings)}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
