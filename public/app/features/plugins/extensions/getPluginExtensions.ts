import { isString } from 'lodash';

import {
  type PluginExtension,
  PluginExtensionTypes,
  type PluginExtensionLink,
  type PluginExtensionComponent,
} from '@grafana/data';
import { GetPluginExtensions } from '@grafana/runtime';

import type { PluginExtensionRegistries, PluginRegistryStates } from './registry/types';
import {
  getReadOnlyProxy,
  logWarning,
  generateExtensionId,
  wrapWithPluginContext,
  getLinkExtensionOnClick,
  getLinkExtensionOverrides,
  getLinkExtensionPathWithTracking,
} from './utils';

type GetExtensions = ({
  context,
  extensionPointId,
  limitPerPlugin,
  registryStates,
}: {
  context?: object | Record<string | symbol, unknown>;
  extensionPointId: string;
  limitPerPlugin?: number;
  registryStates: PluginRegistryStates;
}) => { extensions: PluginExtension[] };

export function createPluginExtensionsGetter(registries: PluginExtensionRegistries): GetPluginExtensions {
  let registryStates: PluginRegistryStates = {
    addedComponentsRegistry: {},
    addedLinksRegistry: {},
  };

  // Create registry subscriptions to keep an copy of the registry state for use in the non-async
  // plugin extensions getter.
  registries.addedComponentsRegistry.asObservable().subscribe((addedComponentsRegistry) => {
    registryStates.addedComponentsRegistry = addedComponentsRegistry;
  });

  registries.addedLinksRegistry.asObservable().subscribe((addedLinksRegistry) => {
    registryStates.addedLinksRegistry = addedLinksRegistry;
  });

  return (options) => getPluginExtensions({ ...options, registryStates });
}

// Returns with a list of plugin extensions for the given extension point
export const getPluginExtensions: GetExtensions = ({ context, extensionPointId, limitPerPlugin, registryStates }) => {
  const frozenContext = context ? getReadOnlyProxy(context) : {};
  // We don't return the extensions separated by type, because in that case it would be much harder to define a sort-order for them.
  const extensions: PluginExtension[] = [];
  const extensionsByPlugin: Record<string, number> = {};

  for (const addedLink of registryStates.addedLinksRegistry[extensionPointId] ?? []) {
    try {
      const { pluginId } = addedLink;
      // Only limit if the `limitPerPlugin` is set
      if (limitPerPlugin && extensionsByPlugin[pluginId] >= limitPerPlugin) {
        continue;
      }

      if (extensionsByPlugin[pluginId] === undefined) {
        extensionsByPlugin[pluginId] = 0;
      }

      // Run the configure() function with the current context, and apply the ovverides
      const overrides = getLinkExtensionOverrides(pluginId, addedLink, frozenContext);

      // configure() returned an `undefined` -> hide the extension
      if (addedLink.configure && overrides === undefined) {
        continue;
      }

      const path = overrides?.path || addedLink.path;
      const extension: PluginExtensionLink = {
        id: generateExtensionId(pluginId, {
          ...addedLink,
          extensionPointId,
          type: PluginExtensionTypes.link,
        }),
        type: PluginExtensionTypes.link,
        pluginId: pluginId,
        onClick: getLinkExtensionOnClick(pluginId, extensionPointId, addedLink, frozenContext),

        // Configurable properties
        icon: overrides?.icon || addedLink.icon,
        title: overrides?.title || addedLink.title,
        description: overrides?.description || addedLink.description,
        path: isString(path) ? getLinkExtensionPathWithTracking(pluginId, path, extensionPointId) : undefined,
        category: overrides?.category || addedLink.category,
      };

      extensions.push(extension);
      extensionsByPlugin[pluginId] += 1;
    } catch (error) {
      if (error instanceof Error) {
        logWarning(error.message);
      }
    }
  }

  const addedComponents = registryStates.addedComponentsRegistry[extensionPointId] ?? [];
  for (const addedComponent of addedComponents) {
    // Only limit if the `limitPerPlugin` is set
    if (limitPerPlugin && extensionsByPlugin[addedComponent.pluginId] >= limitPerPlugin) {
      continue;
    }

    if (extensionsByPlugin[addedComponent.pluginId] === undefined) {
      extensionsByPlugin[addedComponent.pluginId] = 0;
    }
    const extension: PluginExtensionComponent = {
      id: generateExtensionId(addedComponent.pluginId, {
        ...addedComponent,
        extensionPointId,
        type: PluginExtensionTypes.component,
      }),
      type: PluginExtensionTypes.component,
      pluginId: addedComponent.pluginId,
      title: addedComponent.title,
      description: addedComponent.description,
      component: wrapWithPluginContext(addedComponent.pluginId, addedComponent.component),
    };

    extensions.push(extension);
    extensionsByPlugin[addedComponent.pluginId] += 1;
  }

  return { extensions };
};
