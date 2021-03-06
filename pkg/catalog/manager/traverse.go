package manager

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/rancher/norman/controller"
	helmlib "github.com/rancher/rancher/pkg/catalog/helm"
	cutils "github.com/rancher/rancher/pkg/catalog/utils"
	v3 "github.com/rancher/rancher/pkg/types/apis/management.cattle.io/v3"
	client "github.com/rancher/rancher/pkg/types/client/management/v3"
	"github.com/rancher/rancher/pkg/types/namespace"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func (m *Manager) traverseAndUpdate(helm *helmlib.Helm, commit string, cmt *CatalogInfo) error {
	var templateNamespace string
	catalog := cmt.catalog
	projectCatalog := cmt.projectCatalog
	clusterCatalog := cmt.clusterCatalog
	catalogType := getCatalogType(cmt)

	index, err := helm.LoadIndex()
	if err != nil {
		return err
	}

	switch catalogType {
	case client.CatalogType:
		templateNamespace = namespace.GlobalNamespace
	case client.ClusterCatalogType:
		templateNamespace = clusterCatalog.Namespace
	case client.ProjectCatalogType:
		templateNamespace = projectCatalog.Namespace
	}

	newHelmVersionCommits := map[string]v3.VersionCommits{}
	var errs []error
	var updateErrors []error
	var createErrors []error
	var createdTemplates, updatedTemplates, deletedTemplates, failedTemplates int

	if (v3.CatalogConditionRefreshed.IsUnknown(catalog) && !strings.Contains(v3.CatalogConditionRefreshed.GetStatus(catalog), "syncing catalog")) || v3.CatalogConditionRefreshed.IsTrue(catalog) || catalog.Status.Conditions == nil {
		cmt, err = m.updateCatalogInfo(cmt, catalogType, "sync", true, false)
		if err != nil {
			return err
		}
	}

	entriesWithErrors, entriesToProcess := m.preprocessCatalog(catalog, index.IndexFile.Entries)
	for chart, metadata := range entriesToProcess {
		newHelmVersionCommits[chart] = v3.VersionCommits{
			Value: map[string]string{},
		}
		existingHelmVersionCommits := map[string]string{}
		if catalog.Status.HelmVersionCommits[chart].Value != nil {
			existingHelmVersionCommits = catalog.Status.HelmVersionCommits[chart].Value
		}
		keywords := map[string]struct{}{}
		// comparing version commit with the previous commit to detect if a template has been changed.
		hasChanged := false
		versionNumber := 0
		for _, version := range metadata {
			newHelmVersionCommits[chart].Value[version.Version] = version.Digest
			digest, ok := existingHelmVersionCommits[version.Version]
			if !ok || digest != version.Digest {
				hasChanged = true
			}
			if ok {
				versionNumber++
			}
		}
		// if there is a version getting deleted then also set hasChanged to true
		if versionNumber != len(existingHelmVersionCommits) {
			hasChanged = true
		}

		if !hasChanged && hasAllUpdates(catalog) {
			logrus.Debugf("chart %s has not been changed. Skipping generating templates for it", chart)
			continue
		}

		template := v3.CatalogTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name: strings.ToLower(chart),
			},
		}
		template.Namespace = templateNamespace
		template.Spec.Description = metadata[0].Description
		template.Spec.DefaultVersion = metadata[0].Version
		if len(metadata[0].Sources) > 0 {
			template.Spec.ProjectURL = metadata[0].Sources[0]
		}
		iconFilename, iconURL, err := helm.Icon(metadata)
		if err != nil {
			return err
		}

		template.Spec.Icon = iconURL
		template.Spec.IconFilename = iconFilename
		template.Spec.FolderName = chart
		template.Spec.DisplayName = chart
		template.Status.HelmVersion = catalog.Spec.HelmVersion
		label := map[string]string{}
		var versions []v3.TemplateVersionSpec
		for _, version := range metadata {
			v := v3.TemplateVersionSpec{
				Version: strings.ToLower(version.Version),
			}

			files, err := helm.FetchLocalFiles(version)
			if err != nil {
				return err
			}

			for _, file := range files {
				for _, f := range supportedFiles {
					if strings.EqualFold(fmt.Sprintf("%s/%s", chart, f), file.Name) {
						var value catalogYml
						if err := yaml.Unmarshal([]byte(file.Contents), &value); err != nil {
							errs = append(errs, err)
							continue
						}
						v.RancherMinVersion = value.RancherMin
						v.RancherMaxVersion = value.RancherMax
						v.RequiredNamespace = value.Namespace
						label = labels.Merge(label, value.Labels)
						for _, category := range value.Categories {
							keywords[category] = struct{}{}
						}
						break
					}
				}
			}
			v.KubeVersion = version.KubeVersion
			v.Digest = version.Digest

			// for local cache rebuild
			v.VersionDir = version.Dir
			v.VersionName = version.Name
			v.VersionURLs = version.URLs

			for _, versionSpec := range template.Spec.Versions {
				// only set UpgradeVersionLinks once and not every loop
				if v.UpgradeVersionLinks == nil {
					v.UpgradeVersionLinks = map[string]string{}
				}
				if showUpgradeLinks(v.Version, versionSpec.Version) {
					version := versionSpec.Version
					v.UpgradeVersionLinks[versionSpec.Version] = fmt.Sprintf("%s-%s", template.Name, version)
				}
			}

			if catalogType == client.CatalogType {
				v.ExternalID = fmt.Sprintf(cutils.CatalogExternalIDFormat, catalog.Name, template.Spec.FolderName, v.Version)
			} else {
				v.ExternalID = fmt.Sprintf("catalog://?catalog=%s/%s&type=%s&template=%s&version=%s", templateNamespace, catalog.Name, catalogType, template.Spec.FolderName, v.Version)
			}
			versions = append(versions, v)
		}
		var categories []string
		for k := range keywords {
			categories = append(categories, k)
		}
		// merge all labels from templateVersion to template
		template.Labels = label
		template.Spec.Categories = categories
		template.Spec.Versions = versions
		template.Name = getValidTemplateName(catalog.Name, template.Spec.FolderName)
		switch catalogType {
		case client.CatalogType:
			template.Spec.CatalogID = catalog.Name
		case client.ClusterCatalogType:
			if clusterCatalog == nil || clusterCatalog.Name == "" {
				return errors.New("Cluster catalog is no longer available")
			}
			labelMap := make(map[string]string)
			cname := clusterCatalog.Namespace + ":" + clusterCatalog.Name
			template.Spec.ClusterCatalogID = cname
			template.Spec.ClusterID = clusterCatalog.ClusterName
			labelMap[template.Spec.ClusterID+"-"+clusterCatalog.Name] = clusterCatalog.Name
			newLabels := labels.Merge(template.Labels, labelMap)
			template.Labels = newLabels
		case client.ProjectCatalogType:
			if projectCatalog == nil || projectCatalog.Name == "" {
				return errors.New("Project catalog is no longer available")
			}
			labelMap := make(map[string]string)
			pname := projectCatalog.Namespace + ":" + projectCatalog.Name
			template.Spec.ProjectCatalogID = pname
			template.Spec.ProjectID = projectCatalog.ProjectName
			split := strings.SplitN(template.Spec.ProjectID, ":", 2)
			if len(split) != 2 {
				return errors.New("Project ID invalid while creating template")
			}
			labelMap[split[0]+"-"+projectCatalog.Namespace+"-"+projectCatalog.Name] = projectCatalog.Name
			newLabels := labels.Merge(template.Labels, labelMap)
			template.Labels = newLabels
		}

		catalog = cmt.catalog
		projectCatalog = cmt.projectCatalog
		clusterCatalog = cmt.clusterCatalog
		if catalog == nil || catalog.Name == "" {
			return errors.New("Catalog is no longer available")
		}
		if isUpToDate(commit, catalog) {
			logrus.Debugf("Stopping catalog [%s] update, catalog already up to date", catalog.Name)
			return nil
		}
		logrus.Debugf("Catalog [%s] found chart template for [%s]", catalog.Name, chart)

		// look for template by name, if not found then create it, otherwise do update
		existing, err := m.templateLister.Get(template.Namespace, template.Name)
		if apierrors.IsNotFound(err) {
			err = m.createTemplate(template, catalog)
			if err != nil {
				createErrors = append(createErrors, err)
				failedTemplates++
			} else {
				createdTemplates++
			}
		} else if err == nil {
			err = m.updateTemplate(existing, template)
			if err != nil {
				updateErrors = append(updateErrors, err)
				failedTemplates++
			} else {
				updatedTemplates++
			}
		}
		if err != nil {
			delete(newHelmVersionCommits, template.Spec.DisplayName)
		}
	}

	toDeleteChart := []string{}
	for chart := range catalog.Status.HelmVersionCommits {
		if _, ok := index.IndexFile.Entries[chart]; !ok {
			toDeleteChart = append(toDeleteChart, getValidTemplateName(catalog.Name, chart))
		}
	}
	// delete non-existing templates
	for _, toDelete := range toDeleteChart {
		logrus.Debugf("Deleting template %s and its associated templateVersion in namespace %s", toDelete, templateNamespace)
		if err := m.deleteChart(toDelete, templateNamespace); err != nil {
			return err
		}
		deletedTemplates++
	}
	failedTemplates = failedTemplates + len(entriesWithErrors)
	logrus.Infof("Catalog sync done. %v templates created, %v templates updated, %v templates deleted, %v templates failed", createdTemplates, updatedTemplates, deletedTemplates, failedTemplates)

	catalog.Status.HelmVersionCommits = newHelmVersionCommits

	if projectCatalog != nil {
		projectCatalog.Catalog = *catalog
	} else if clusterCatalog != nil {
		clusterCatalog.Catalog = *catalog
	}
	/*conditions need to be set here to stop templates from updating
	each time when they have no changes
	*/
	setTraverseCompleted(catalog)
	var errstrings []string
	if len(createErrors) > 0 {
		errstrings = append(errstrings, fmt.Sprintf("failed to create templates. Multiple error(s) occurred: %v", createErrors))
	}
	if len(updateErrors) > 0 {
		errstrings = append(errstrings, fmt.Sprintf("failed to update templates. Multiple error(s) occurred: %v", updateErrors))
	}
	if len(entriesWithErrors) > 0 && len(errstrings) == 0 {
		invalidChartErrors := processInvalidChartErrors(entriesWithErrors)
		setCatalogIgnoreErrorState(commit, cmt, catalog, projectCatalog, clusterCatalog, fmt.Sprintf("Error in chart(s): %s", invalidChartErrors))
		if _, err := m.updateCatalogInfo(cmt, catalogType, "", false, true); err != nil {
			return err
		}
		logrus.Error(fmt.Sprintf("failed to sync templates. Multiple error(s) occured: %v", invalidChartErrors))
		return &controller.ForgetError{Err: errors.Errorf("failed to sync templates. Multiple error(s) occurred: %v", invalidChartErrors)}
	}
	if len(errstrings) > 0 {
		invalidChartErrors := processInvalidChartErrors(entriesWithErrors)
		errstrings = append(errstrings, invalidChartErrors)
		setCatalogErrorState(cmt, catalog, projectCatalog, clusterCatalog)
		if _, err := m.updateCatalogInfo(cmt, catalogType, "", false, true); err != nil {
			return err
		}
		return errors.Errorf(strings.Join(errstrings, ";"))
	}
	var finalError error
	if len(errs) > 0 {
		finalError = errors.Errorf("failed to sync templates. Resetting commit. Multiple error occurred: %v", errs)
		commit = ""
	}

	catalog.Status.Commit = commit
	if projectCatalog != nil {
		projectCatalog.Catalog = *catalog
	} else if clusterCatalog != nil {
		clusterCatalog.Catalog = *catalog
	}
	cmt.catalog = catalog
	cmt.projectCatalog = projectCatalog
	cmt.clusterCatalog = clusterCatalog
	if _, err := m.updateCatalogInfo(cmt, catalogType, "", true, true); err != nil {
		return err
	}

	return finalError
}

var supportedFiles = []string{"catalog.yml", "catalog.yaml", "questions.yml", "questions.yaml"}

type catalogYml struct {
	RancherMin string            `yaml:"rancher_min_version,omitempty"`
	RancherMax string            `yaml:"rancher_max_version,omitempty"`
	Categories []string          `yaml:"categories,omitempty"`
	Namespace  string            `yaml:"namespace,omitempty"`
	Labels     map[string]string `yaml:"labels,omitempty"`
}

func processInvalidChartErrors(entriesWithErrors []ChartsWithErrors) string {
	var invalidChartErrors strings.Builder
	for _, errorInfo := range entriesWithErrors {
		fmt.Fprintf(&invalidChartErrors, "%s;", errorInfo.error.Error())
	}
	return invalidChartErrors.String()
}
