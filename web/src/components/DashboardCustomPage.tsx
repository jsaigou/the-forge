import { useState } from "react";
import { ErrorBoundary } from "./ErrorBoundary";
import {
  getWidget,
  listWidgets,
  resolveWidgetProps,
  type WidgetProps,
  type WidgetRegistryEntry,
} from "../lib/widgetRegistry";
import type { DashboardPage, WidgetInstance } from "../lib/types";

// ADR-0011/0012: renders a custom dashboard page — an ordered stack of
// widget instances resolved by slug from the registry. In edit mode (admin
// only), each widget gets controls: drag-to-reorder, per-instance config
// (auto-rendered from configSchema), and remove. An "Add Widget" gallery
// lets the operator place any registered widget.

interface DashboardCustomPageProps {
  page: DashboardPage;
  canEdit: boolean;
  onUpdatePage: (page: DashboardPage) => void;
}

export function DashboardCustomPage({ page, canEdit, onUpdatePage }: DashboardCustomPageProps) {
  const [editing, setEditing] = useState(false);
  const [draggedIndex, setDraggedIndex] = useState<number | null>(null);
  const [configuringIndex, setConfiguringIndex] = useState<number | null>(null);
  const [showGallery, setShowGallery] = useState(false);

  function updateWidget(index: number, props: WidgetProps) {
    const widgets = [...page.widgets];
    widgets[index] = { ...widgets[index], props };
    onUpdatePage({ ...page, widgets });
  }

  function removeWidget(index: number) {
    const widgets = page.widgets.filter((_, i) => i !== index);
    onUpdatePage({ ...page, widgets });
  }

  function addWidget(slug: string) {
    const entry = getWidget(slug);
    if (!entry) return;
    const props: WidgetProps = {};
    for (const prop of entry.configSchema) {
      props[prop.name] = prop.default;
    }
    const widget: WidgetInstance = { slug, props };
    onUpdatePage({ ...page, widgets: [...page.widgets, widget] });
    setShowGallery(false);
  }

  function reorderWidgets(from: number, to: number) {
    const widgets = [...page.widgets];
    const [moved] = widgets.splice(from, 1);
    widgets.splice(to, 0, moved);
    onUpdatePage({ ...page, widgets });
  }

  return (
    <>
      {canEdit && (
        <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10 }}>
          <button className={`tab ${editing ? "active" : ""}`} onClick={() => setEditing(!editing)}>
            {editing ? "Done" : "Edit"}
          </button>
        </div>
      )}
      {page.widgets.length === 0 && (
        <div className="empty-note">
          {editing ? 'No widgets yet. Use "Add Widget" below.' : "This page is empty."}
        </div>
      )}
      {page.widgets.map((widget, index) => {
        const entry = getWidget(widget.slug);
        if (!entry) {
          return (
            <div className="card" key={index}>
              <div className="empty-note">Unknown widget: {widget.slug}</div>
            </div>
          );
        }
        const resolvedProps = resolveWidgetProps(entry, widget.props);
        const Component = entry.component;
        return (
          <div
            key={index}
            draggable={editing}
            onDragStart={editing ? () => setDraggedIndex(index) : undefined}
            onDragOver={editing ? (e) => e.preventDefault() : undefined}
            onDrop={
              editing && draggedIndex != null
                ? () => {
                    reorderWidgets(draggedIndex, index);
                    setDraggedIndex(null);
                  }
                : undefined
            }
            style={
              editing
                ? {
                    position: "relative",
                    border: "1px dashed var(--border)",
                    borderRadius: 8,
                    padding: 4,
                    marginBottom: 4,
                    opacity: draggedIndex === index ? 0.4 : 1,
                  }
                : undefined
            }
          >
            {editing && (
              <div style={{ display: "flex", justifyContent: "flex-end", gap: 4, marginBottom: 4 }}>
                {entry.configSchema.length > 0 && (
                  <button
                    className="btn"
                    style={{ fontSize: 11, padding: "2px 8px" }}
                    title="Configure"
                    onClick={() => setConfiguringIndex(configuringIndex === index ? null : index)}
                  >
                    Config
                  </button>
                )}
                <button
                  className="btn"
                  style={{ fontSize: 11, padding: "2px 8px" }}
                  title="Remove"
                  onClick={() => removeWidget(index)}
                >
                  Remove
                </button>
              </div>
            )}
            {configuringIndex === index && (
              <WidgetConfigForm
                entry={entry}
                currentProps={widget.props}
                onSave={(props) => {
                  updateWidget(index, props);
                  setConfiguringIndex(null);
                }}
                onCancel={() => setConfiguringIndex(null)}
              />
            )}
            <ErrorBoundary>
              <Component {...resolvedProps} />
            </ErrorBoundary>
          </div>
        );
      })}
      {editing && (
        <>
          {showGallery ? (
            <div className="card">
              <div className="eyebrow">Add widget</div>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
                {listWidgets().map((entry) => (
                  <button key={entry.slug} className="btn" style={{ fontSize: 12 }} onClick={() => addWidget(entry.slug)}>
                    {entry.displayName}
                  </button>
                ))}
                <button className="btn" style={{ fontSize: 12 }} onClick={() => setShowGallery(false)}>
                  Cancel
                </button>
              </div>
            </div>
          ) : (
            <button className="btn" onClick={() => setShowGallery(true)}>
              + Add Widget
            </button>
          )}
        </>
      )}
    </>
  );
}

function WidgetConfigForm({
  entry,
  currentProps,
  onSave,
  onCancel,
}: {
  entry: WidgetRegistryEntry;
  currentProps: WidgetProps;
  onSave: (props: WidgetProps) => void;
  onCancel: () => void;
}) {
  const [props, setProps] = useState<WidgetProps>({ ...currentProps });

  return (
    <div className="card" style={{ marginBottom: 8 }}>
      <div className="eyebrow">Configure {entry.displayName}</div>
      {entry.configSchema.map((prop) => (
        <div key={prop.name} style={{ marginBottom: 8 }}>
          <label style={{ fontSize: 11, color: "var(--text-mute)", display: "block", marginBottom: 4 }}>
            {prop.name}
          </label>
          <select
            value={(props[prop.name] as string) ?? prop.default}
            onChange={(e) => setProps({ ...props, [prop.name]: e.target.value })}
            style={{ fontSize: 12 }}
          >
            {prop.options.map((opt) => (
              <option key={opt.key} value={opt.key}>
                {opt.label}
              </option>
            ))}
          </select>
        </div>
      ))}
      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn" onClick={() => onSave(props)}>
          Save
        </button>
        <button className="btn" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}
