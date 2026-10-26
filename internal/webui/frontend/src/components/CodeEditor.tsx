import { useEffect, useRef } from "react";
import { EditorState, Compartment } from "@codemirror/state";
import { EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection } from "@codemirror/view";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { LanguageDescription, bracketMatching, indentOnInput, syntaxHighlighting } from "@codemirror/language";
import { languages } from "@codemirror/language-data";
import { highlightSelectionMatches, searchKeymap } from "@codemirror/search";
import { classHighlighter } from "@lezer/highlight";

// The editor of the file panel. It is its own chunk: the page loads it the
// first time a file is opened, and each language loads by file name.

const theme = EditorView.theme({
  "&": { height: "100%", fontSize: "12.5px", backgroundColor: "transparent", color: "var(--ink)" },
  ".cm-scroller": { fontFamily: "var(--font-mono)", lineHeight: "1.55" },
  ".cm-content": { caretColor: "var(--ink)" },
  ".cm-gutters": { backgroundColor: "transparent", color: "var(--ink-3)", border: "none" },
  ".cm-activeLine, .cm-activeLineGutter": { backgroundColor: "var(--hover)" },
  "&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection": { backgroundColor: "color-mix(in srgb, #3b82f6 28%, transparent) !important" },
  "&.cm-focused": { outline: "none" },
  ".cm-searchMatch": { backgroundColor: "color-mix(in srgb, #f59e0b 30%, transparent)" },
  ".cm-panels": { backgroundColor: "var(--surface)", color: "var(--ink)", borderTop: "1px solid var(--line)" },
});

interface Props {
  value: string;
  filename: string;
  readOnly?: boolean;
  onChange: (value: string) => void;
  onSave: () => void;
}

export default function CodeEditor({ value, filename, readOnly, onChange, onSave }: Props) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const callbacks = useRef({ onChange, onSave });
  callbacks.current = { onChange, onSave };
  const language = useRef(new Compartment());

  useEffect(() => {
    if (!host.current) return;
    const editor = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: value,
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          highlightActiveLine(),
          drawSelection(),
          history(),
          indentOnInput(),
          bracketMatching(),
          highlightSelectionMatches(),
          syntaxHighlighting(classHighlighter),
          language.current.of([]),
          EditorState.readOnly.of(!!readOnly),
          keymap.of([
            { key: "Mod-s", preventDefault: true, run: () => (callbacks.current.onSave(), true) },
            indentWithTab,
            ...defaultKeymap,
            ...historyKeymap,
            ...searchKeymap,
          ]),
          EditorView.updateListener.of((update) => {
            if (update.docChanged) callbacks.current.onChange(update.state.doc.toString());
          }),
          theme,
        ],
      }),
    });
    view.current = editor;
    return () => {
      editor.destroy();
      view.current = null;
    };
    // The document is replaced through the effect below, not by remounting.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [readOnly]);

  // A reload from disk replaces the document.
  useEffect(() => {
    const editor = view.current;
    if (editor && editor.state.doc.toString() !== value) {
      editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: value } });
    }
  }, [value]);

  useEffect(() => {
    let cancelled = false;
    const description = LanguageDescription.matchFilename(languages, filename);
    if (!description) {
      view.current?.dispatch({ effects: language.current.reconfigure([]) });
      return;
    }
    description.load().then((support) => {
      if (!cancelled) view.current?.dispatch({ effects: language.current.reconfigure(support) });
    });
    return () => {
      cancelled = true;
    };
  }, [filename]);

  return <div ref={host} className="h-full min-h-0 overflow-hidden" />;
}
