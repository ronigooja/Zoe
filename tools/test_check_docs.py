"""Self-contained structural documentation regressions; no repository doc fixtures."""
import contextlib
import io
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import check_docs


class DocumentationChecks(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        entries = ('docs/design/data-flows.md', 'docs/development.md', 'docs/operations.md')
        for name in ('AGENTS.md', 'README.md'):
            self.write(name, '# Entry\n' + ''.join(f'[Guide]({entry})\n' for entry in entries))
        self.write('docs/development.md', '# Development\n')
        self.write('docs/operations.md', '# Operations\n')
        self.write('docs/design/data-flows.md', '# Data flows\n'
                   '[Development](../development.md)\n[Operations](../operations.md)\n')
        for name in check_docs.REQUIRED_COMPONENTS:
            self.add_contract(name)

    def write(self, relative, text):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding='utf-8')

    def append(self, relative, text):
        path = self.root / relative
        path.write_text(path.read_text(encoding='utf-8') + text, encoding='utf-8')

    def add_contract(self, name, *, listed=True):
        self.write(f'docs/design/flows/{name}.md', f'# {name}\n' + ''.join(
            f'\n## {heading}\nContract content.\n' for heading in check_docs.CONTRACT_HEADINGS))
        if listed:
            self.append('docs/design/data-flows.md', f'[{name}](flows/{name}.md)\n')

    def run_main(self, *flags):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), mock.patch(
            'sys.argv', ['check_docs.py', '--root', str(self.root), *flags]
        ):
            result = check_docs.main()
        return result, output.getvalue()

    def test_valid_extensible_structure(self):
        self.add_contract('future-component')
        self.append('docs/design/flows/future-component.md', '\n## Rationale\nOptional context.\n')
        self.assertEqual(self.run_main('--strict-length')[0], 0)

    def test_missing_required_entries(self):
        for relative in check_docs.REQUIRED_DOCUMENTS:
            with self.subTest(relative=relative):
                path = self.root / relative
                text = path.read_text()
                path.unlink()
                self.assertIn(f'missing required document: {relative}', check_docs.structure_check(self.root))
                self.write(relative, text)

    def test_missing_required_component(self):
        (self.root / 'docs/design/flows/register.md').unlink()
        self.assertTrue(any('missing required document: docs/design/flows/register.md' in error
                            for error in check_docs.structure_check(self.root)))

    def test_missing_entry_navigation(self):
        self.write('AGENTS.md', '# Entry\n')
        self.assertTrue(any('missing navigation in AGENTS.md' in error
                            for error in check_docs.structure_check(self.root)))

    def test_missing_empty_duplicate_contract_section(self):
        path = self.root / 'docs/design/flows/register.md'
        original = path.read_text()
        heading = check_docs.CONTRACT_HEADINGS[0]
        for replacement, message in (
            ('', 'occurs 0 times'),
            (f'## {heading}\n<!-- no body -->\n### Subheading\n', 'empty contract section'),
            (f'## {heading}\nBody.\n## {heading}\nBody.\n', 'occurs 2 times'),
        ):
            with self.subTest(message=message):
                path.write_text(original.replace(f'## {heading}\nContract content.\n', replacement))
                self.assertTrue(any(message in error for error in check_docs.structure_check(self.root)))

    def test_inline_code_is_valid_contract_content(self):
        path = self.root / 'docs/design/flows/register.md'
        path.write_text(path.read_text().replace('Contract content.', '`internal/example.go`'))
        self.assertEqual(check_docs.structure_check(self.root), [])

    def test_new_component_requires_discoverable_navigation(self):
        self.add_contract('future-component', listed=False)
        self.append('docs/design/data-flows.md', '```md\n[Example](flows/future-component.md)\n```\n')
        self.assertTrue(any('missing navigation' in error and 'future-component.md' in error
                            for error in check_docs.structure_check(self.root)))

    def test_broken_file_anchor_and_reference(self):
        for text, message in (
            ('[Missing](missing.md)', 'broken local link'),
            ('[Missing](#missing)', 'broken Markdown heading link'),
            ('[Missing][absent]', 'unresolved Markdown link reference'),
            ('[unused]: missing.md', 'broken local link'),
        ):
            with self.subTest(message=message):
                self.write('index.md', '# Index\n' + text)
                self.assertTrue(any(message in error for error in check_docs.link_check(self.root)))

    def test_backticks_in_destinations_and_titles_are_not_code_spans(self):
        self.write('index.md', '[Missing](docs/`nonexistent`development.md)\n'
                   '[First](docs/development.md "` literal") [Missing](missing.md) '
                   '[Last](docs/operations.md "` literal")\n')
        errors = check_docs.link_check(self.root)
        self.assertEqual(len(errors), 2)
        self.assertTrue(any('docs/`nonexistent`development.md' in error for error in errors))
        self.assertTrue(any(': missing.md' in error for error in errors))
        self.assertIn('docs/operations.md "` literal"', check_docs.link_destinations(
            (self.root / 'index.md').read_text()))

    def test_unused_reference_definitions_do_not_supply_navigation(self):
        for relative in ('AGENTS.md', 'docs/design/data-flows.md'):
            with self.subTest(relative=relative):
                path = self.root / relative
                original = path.read_text()
                destinations = check_docs.link_destinations(original)
                definitions = ''.join(f'[guide-{index}]: {destination}\n'
                                      for index, destination in enumerate(destinations))
                path.write_text('# Entry\n' + definitions)
                self.assertTrue(any(f'missing navigation in {relative}:' in error
                                    for error in check_docs.structure_check(self.root)))
                uses = ''.join(f'[Guide][guide-{index}]\n' for index in range(len(destinations)))
                path.write_text('# Entry\n' + uses + definitions)
                self.assertEqual(check_docs.structure_check(self.root), [])
                self.assertEqual(check_docs.link_check(self.root), [])
                path.write_text(original)

    def test_unused_reference_definitions_still_checked_for_retired_links(self):
        self.write('index.md', '[old]: docs/product.md\n')
        self.assertTrue(any('retired documentation link' in error
                            for error in check_docs.authority_check(self.root)))

    def test_fence_closing_rules_are_shared(self):
        for opening, invalid, closing in (
            ('```python', '```not-a-closing-fence', '````  '),
            ('~~~~', '~~~', '~~~~\t'),
            ('```', '~~~', '```'),
        ):
            with self.subTest(opening=opening, invalid=invalid):
                text = (opening + '\n' + invalid + '\n# Hidden\n'
                        '[Hidden](missing.md)\n中文\n'
                        + 'x' * (check_docs.PARAGRAPH_LINE_LIMIT + 1) + '\n'
                        + closing + '\n# Visible\n')
                self.write('index.md', text)
                self.assertEqual(check_docs.heading_anchors(text), {'visible'})
                self.assertEqual(check_docs.navigation_text(text), '# Visible')
                self.assertEqual(check_docs.link_check(self.root), [])
                self.assertEqual(check_docs.cjk_check(self.root), [])
                self.assertEqual(check_docs.length_warnings(self.root), [])

    def test_contract_sections_hidden_in_unclosed_fence_do_not_count(self):
        path = self.root / 'docs/design/flows/register.md'
        path.write_text('```markdown\n```not-a-closing-fence\n' + path.read_text())
        errors = check_docs.structure_check(self.root)
        self.assertEqual(len(errors), len(check_docs.CONTRACT_HEADINGS))
        self.assertTrue(all('occurs 0 times' in error for error in errors))

    def test_valid_anchors_references_and_ignored_examples(self):
        self.write('index.md', '# Same\n# Same\n<a id="explicit"></a>\n'
                   '[First](#same) [Second](#same-1) [Explicit](#explicit)\n'
                   '[Reference][target]\n[target]: #same\n'
                   '```\n# Ignored\n[Example](missing.md)\n```\n'
                   '`[Example](missing.md)`\n<!-- [Example](missing.md) -->\n')
        self.assertEqual(check_docs.link_check(self.root), [])
        self.assertNotIn('ignored', check_docs.heading_anchors((self.root / 'index.md').read_text()))

    def test_github_anchor_punctuation_and_duplicate_rules(self):
        text = ('## Repair — 2026-10-29\n## Repair — 2026-10-29\n'
                '## `Colon`:  multiple  spaces\n')
        self.assertEqual(check_docs.heading_anchors(text), {
            'repair--2026-10-29', 'repair--2026-10-29-1', 'colon-multiple-spaces'})

    def test_retired_and_archive_links_fail_even_when_files_exist(self):
        for relative in ('docs/product.md', 'docs/acceptance/evidence.md',
                         'docs/architecture/decisions/example.md', 'docs/archive/old.md',
                         'docs/history/old.md'):
            with self.subTest(relative=relative):
                self.write(relative, '# Old\n')
                self.write('index.md', f'[Old]({relative})\n')
                self.assertTrue(check_docs.authority_check(self.root))

    def test_immutable_external_history_links_are_not_local_authority(self):
        self.append('README.md', '[Historical source](https://github.com/example/zoe/blob/'
                    + 'a' * 40 + '/docs/product.md#old)\n')
        self.assertEqual(check_docs.authority_check(self.root), [])
        self.assertEqual(check_docs.link_check(self.root), [])

    def test_bare_retired_document_reference_fails(self):
        self.append('README.md', 'Follow `docs/product.md` for requirements.\n')
        self.assertTrue(any('retired documentation reference' in error
                            for error in check_docs.authority_check(self.root)))

    def test_english_narrative_and_source_literals(self):
        self.write('index.md', '中文叙述\n')
        self.assertTrue(check_docs.cjk_check(self.root))
        self.write('index.md', 'English `中文` literal.\n```\n中文\n```\n> 中文 quotation\n')
        self.assertEqual(check_docs.cjk_check(self.root), [])

    def test_length_warning_and_strict_mode(self):
        self.write('index.md', 'x' * (check_docs.PARAGRAPH_LINE_LIMIT + 1) + '\n'
                   '| A | B |\n|---|---|\n| short | '
                   + 'y' * (check_docs.TABLE_CELL_LIMIT + 1) + ' |\n')
        self.assertEqual(len(check_docs.length_warnings(self.root)), 2)
        self.assertEqual(self.run_main()[0], 0)
        result, output = self.run_main('--strict-length')
        self.assertEqual(result, 1)
        self.assertIn('WARN: long paragraph line', output)


if __name__ == '__main__':
    unittest.main()
