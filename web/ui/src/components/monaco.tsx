import { useDarkTheme } from "../theme";
import type { CodeEditorProps } from "./CodeEditor";

import Editor, { loader, type OnMount } from "@monaco-editor/react";
import * as monaco from "monaco-editor";
import EditorWorker from "monaco-editor/editor/editor.worker?worker";
import TsWorker from "monaco-editor/language/typescript/ts.worker?worker";
import { useEffect, useRef } from "react";

self.MonacoEnvironment = {
    getWorker(_id, label) {
        return label === "typescript" || label === "javascript"
            ? new TsWorker()
            : new EditorWorker();
    },
};
loader.config({ monaco });

export default function MonacoEditor({
    value,
    onChange,
    language,
    height = "300px",
}: CodeEditorProps) {
    // Follows html[data-theme] like the rest of the app, not the OS media
    // query directly, so it can never disagree with the page around it.
    const dark = useDarkTheme();
    // Uncontrolled: the library's controlled `value` replaces the whole
    // document whenever the prop lags the model (i.e. while typing), which
    // throws the cursor around. Only push values that came from outside,
    // e.g. content fetched after mount.
    const editor = useRef<Parameters<OnMount>[0]>(null);
    const emitted = useRef(value);
    useEffect(() => {
        if (editor.current && value !== emitted.current && value !== editor.current.getValue()) {
            editor.current.setValue(value);
        }
        emitted.current = value;
    }, [value]);
    return (
        <Editor
            height={height}
            language={language}
            theme={dark ? "vs-dark" : "light"}
            defaultValue={value}
            onMount={(e) => {
                editor.current = e;
                // value may have changed while Monaco was loading.
                if (e.getValue() !== emitted.current) {
                    e.setValue(emitted.current);
                }
            }}
            onChange={(v) => {
                emitted.current = v ?? "";
                onChange(emitted.current);
            }}
            options={{
                minimap: { enabled: false },
                scrollBeyondLastLine: false,
                fontSize: 13,
                wordWrap: "on",
            }}
        />
    );
}
