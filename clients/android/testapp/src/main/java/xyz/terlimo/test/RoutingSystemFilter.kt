package xyz.terlimo.test

import android.content.pm.ApplicationInfo

/**
 * UI-1a display-only filter and quick-selection helpers for the routing editor.
 *
 * The filter only decides which rows are rendered. It never mutates the persisted
 * selection: a system package hidden because the filter is off stays in `checked`
 * and is therefore still written by Save. Quick selection acts on the installed
 * matching packages regardless of the current visibility.
 */
internal object RoutingSystemFilter {
    fun isVisible(isSystem: Boolean, showSystem: Boolean): Boolean = showSystem || !isSystem

    /** Robust system-app classification: preinstalled or updated-system app. */
    fun isSystem(flags: Int): Boolean =
        (flags and ApplicationInfo.FLAG_SYSTEM) != 0 ||
            (flags and ApplicationInfo.FLAG_UPDATED_SYSTEM_APP) != 0

    /**
     * Installed quick-selection matches that are not already selected.
     * Independent of the display filter and of which rows are visible.
     */
    fun quickAdditions(
        installed: Set<String>,
        quick: Set<String>,
        checked: Set<String>,
    ): Set<String> = quick.intersect(installed) - checked
}
