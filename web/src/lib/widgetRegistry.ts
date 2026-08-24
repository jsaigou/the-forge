import type { ComponentType } from "react";
import { ActivityHeatmapWidget } from "../components/widgets/ActivityHeatmapWidget";
import { ElectricityBreakdownWidget } from "../components/widgets/ElectricityBreakdownWidget";
import { MemoryMeterWidget } from "../components/widgets/MemoryMeterWidget";
import { PerModelSpendTableWidget } from "../components/widgets/PerModelSpendTableWidget";
import { ResourceGaugesWidget } from "../components/widgets/ResourceGaugesWidget";
import { ResourceTrendWidget } from "../components/widgets/ResourceTrendWidget";
import { RightNowWidget } from "../components/widgets/RightNowWidget";

// ADR-0012: the widget registry declares a configSchema per widget type.
// The page editor auto-renders a per-instance config form from the schema.
// v1 supports "select" prop type (for window_). Future types (number,
// boolean, text) and future widgets declaring their own schemas are
// additive — no registry redesign needed.

export interface ConfigSchemaProp {
  name: string;
  type: "select";
  default: string;
  options: readonly { key: string; label: string }[];
}

export type WidgetProps = Record<string, string | number | boolean>;

// Components in the registry have heterogeneous prop signatures (some take
// no props, some take { window_: string }). The loose type is intentional;
// the configSchema provides the actual per-prop type safety for the editor,
// and the rendering layer spreads resolved props (from configSchema defaults)
// onto the component.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type WidgetComponent = ComponentType<any>;

export interface WidgetRegistryEntry {
  slug: string;
  displayName: string;
  component: WidgetComponent;
  configSchema: ConfigSchemaProp[];
}

const RESOURCE_WINDOWS = [
  { key: "24h", label: "24h" },
  { key: "72h", label: "72h" },
  { key: "7d", label: "1w" },
  { key: "30d", label: "1m" },
] as const;

const COST_WINDOWS = [
  { key: "30d", label: "1 Month" },
  { key: "180d", label: "6 Months" },
  { key: "365d", label: "1 Year" },
  { key: "3650d", label: "All Time" },
] as const;

const RIGHT_NOW_WINDOWS = [
  { key: "24h", label: "24h" },
  { key: "72h", label: "72h" },
  { key: "7d", label: "1w" },
] as const;

const REGISTRY: WidgetRegistryEntry[] = [
  {
    slug: "activity-heatmap",
    displayName: "Activity Heatmap",
    component: ActivityHeatmapWidget,
    configSchema: [],
  },
  {
    slug: "right-now",
    displayName: "Right Now",
    component: RightNowWidget,
    configSchema: [
      { name: "window_", type: "select", default: "24h", options: RIGHT_NOW_WINDOWS },
    ],
  },
  {
    slug: "memory-meter",
    displayName: "Memory Meter",
    component: MemoryMeterWidget,
    configSchema: [],
  },
  {
    slug: "resource-gauges",
    displayName: "Resource Gauges",
    component: ResourceGaugesWidget,
    configSchema: [],
  },
  {
    slug: "resource-trend",
    displayName: "Resource Trend",
    component: ResourceTrendWidget,
    configSchema: [
      { name: "window_", type: "select", default: "24h", options: RESOURCE_WINDOWS },
    ],
  },
  {
    slug: "electricity-breakdown",
    displayName: "Electricity Breakdown",
    component: ElectricityBreakdownWidget,
    configSchema: [
      { name: "window_", type: "select", default: "24h", options: RESOURCE_WINDOWS },
    ],
  },
  {
    slug: "per-model-spend",
    displayName: "Per-Model Spend",
    component: PerModelSpendTableWidget,
    configSchema: [
      { name: "window_", type: "select", default: "30d", options: COST_WINDOWS },
    ],
  },
];

const REGISTRY_MAP = new Map(REGISTRY.map((e) => [e.slug, e]));

export function getWidget(slug: string): WidgetRegistryEntry | undefined {
  return REGISTRY_MAP.get(slug);
}

export function listWidgets(): WidgetRegistryEntry[] {
  return REGISTRY;
}

// Resolves a widget instance's props by applying configSchema defaults
// for any props not explicitly set.
export function resolveWidgetProps(entry: WidgetRegistryEntry, props: WidgetProps): WidgetProps {
  const resolved: WidgetProps = {};
  for (const prop of entry.configSchema) {
    resolved[prop.name] = props[prop.name] ?? prop.default;
  }
  return resolved;
}
